package scanner

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhowden/tag"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// fakeAudio is file content the scanner takes as MP3: an empty ID3v2 tag
// followed by s. Tests that need a file to be indexed, not decoded, use it.
func fakeAudio(s string) []byte {
	return append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), s...)
}

// mpegFrames is n MPEG-1 Layer III frames at 128 kbps, 44.1 kHz (417 bytes
// each), with a junk prefix.
func mpegFrames(prefix string, n int) []byte {
	b := []byte(prefix)
	for range n {
		f := make([]byte, 417)
		copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
		b = append(b, f...)
	}
	return b
}

func oggFirstPage(packet string) []byte {
	b := make([]byte, 27)
	copy(b, "OggS")
	b[26] = 1 // one segment
	b = append(b, byte(len(packet)))
	return append(b, packet...)
}

// Every format in audioExts is recognised by its header, and non-audio
// content is not (#219).
func TestSniffFormat(t *testing.T) {
	riff := func(form string) []byte { return append([]byte("RIFF\x00\x00\x00\x00"), form...) }
	for name, c := range map[string]struct {
		data []byte
		kind string
	}{
		"id3 tag":           {fakeAudio("whatever follows"), kindMP3},
		"bare mpeg frames":  {mpegFrames("", 3), kindMP3},
		"frames after junk": {mpegFrames("\x00\x00junk", 3), kindMP3},
		"flac":              {[]byte("fLaC\x00\x00\x00\x22"), kindFLAC},
		"id3 then flac":     {append(fakeAudio(""), "fLaC\x00\x00\x00\x22"...), kindFLAC},
		"ogg vorbis":        {oggFirstPage("\x01vorbis\x00\x00"), kindOggVorbis},
		"ogg opus":          {oggFirstPage("OpusHead\x01"), kindOggOpus},
		"ogg flac":          {oggFirstPage("\x7fFLAC\x01\x00"), kindOggOther},
		"mp4":               {[]byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00"), kindMP4},
		"adts":              {[]byte{0xFF, 0xF1, 0x50, 0x80, 0x02, 0x1F, 0xFC}, kindADTS},
		"wav":               {riff("WAVEfmt "), kindWAV},
		"aiff":              {[]byte("FORM\x00\x00\x00\x00AIFFCOMM"), kindAIFF},
		"aifc":              {[]byte("FORM\x00\x00\x00\x00AIFCFVER"), kindAIFF},
		"wavpack":           {[]byte("wvpk\x00\x00\x00\x00"), kindWavPack},
		"musepack sv8":      {[]byte("MPCKSH"), kindMusepack},
		"musepack sv7":      {[]byte("MP+\x07"), kindMusepack},
		"ape":               {[]byte("MAC \x96\x0f"), kindAPE},
		"asf":               {append(append([]byte{}, asfGUID...), 0, 0), kindASF},
		"text":              {[]byte("root:x:0:0:root:/root:/bin/sh\n"), ""},
		"png":               {[]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), ""},
		"jpeg":              {[]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, ""},
		"riff but not wave": {riff("AVI LIST"), ""},
		"empty":             {nil, ""},
		"one stray sync":    {append([]byte("text"), 0xFF, 0xFB, 0x90, 0x00, 'x', 'y'), ""},
	} {
		if got := sniffFormat(bytes.NewReader(c.data)); got != c.kind {
			t.Errorf("%s: sniffed %q, want %q", name, got, c.kind)
		}
	}
}

func TestResolveSuffix(t *testing.T) {
	for _, c := range []struct {
		ext, kind, suffix string
		ok, mismatch      bool
	}{
		{"mp3", kindMP3, "mp3", true, false},
		{"ogg", kindOggOpus, "opus", true, false}, // Opus named .ogg: right type, no warning
		{"opus", kindOggVorbis, "ogg", true, true},
		{"mp3", kindFLAC, "flac", true, true},
		{"aac", kindADTS, "aac", true, false},
		{"m4a", kindMP4, "m4a", true, false},
		{"aif", kindAIFF, "aif", true, false},
		{"mp3", "", "", false, false},
	} {
		suffix, ok, mismatch := resolveSuffix(c.ext, c.kind)
		if suffix != c.suffix || ok != c.ok || mismatch != c.mismatch {
			t.Errorf("%s holding %q: %q %v %v", c.ext, c.kind, suffix, ok, mismatch)
		}
	}
	for ext := range audioExts {
		if len(accepts[ext]) == 0 {
			t.Errorf("no container accepted for .%s", ext)
		}
	}
}

func failureRows(t *testing.T, pool *sql.DB) map[string]string {
	t.Helper()
	rows, err := pool.Query("SELECT path, reason FROM scan_failures")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, r string
		if err := rows.Scan(&p, &r); err != nil {
			t.Fatal(err)
		}
		out[p] = r
	}
	return out
}

// A parser that panics costs the scan one file, not the process. The file is
// recorded, skipped on the next scan, and tried again once it changes; when
// it reads, the record goes (#219).
func TestParserPanicSkipsTheFile(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, covers := t.TempDir(), t.TempDir()
	good := filepath.Join(music, "good.mp3")
	bad := filepath.Join(music, "bad.mp3")
	for _, p := range []string{good, bad} {
		if err := os.WriteFile(p, fakeAudio(filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	defer func(orig func(io.ReadSeeker) (tag.Metadata, error)) { readTags = orig }(readTags)
	readTags = func(r io.ReadSeeker) (tag.Metadata, error) {
		b, _ := io.ReadAll(r)
		if bytes.Contains(b, []byte("bad")) {
			calls++
			var m map[string]int
			m["boom"]++ // a nil-map write, as a buggy parser would do
		}
		return nil, tag.ErrNoTagsFound
	}

	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	stats, err := Scan(pool, []string{music}, covers, nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 || stats.Failed != 1 {
		t.Fatalf("first scan: %+v", stats)
	}
	if got := failureRows(t, pool); !strings.Contains(got[bad], "parser failed") {
		t.Fatalf("failure not recorded: %v", got)
	}
	if n, _ := CountFailures(pool); n != 1 {
		t.Fatalf("CountFailures = %d", n)
	}

	// Unchanged: not parsed again, by the scan or the watcher.
	stats, err = Scan(pool, []string{music}, covers, nil, &p)
	if err != nil || stats.Failed != 1 || calls != 1 {
		t.Fatalf("second scan: %+v %v, parser calls %d", stats, err, calls)
	}
	if res, err := processFile(pool, []string{music}, bad, covers, nil); res != resultFailed || err != nil || calls != 1 {
		t.Fatalf("watcher path: %v %v, parser calls %d", res, err, calls)
	}

	// Changed and now readable: indexed, and the record is gone.
	if err := os.WriteFile(bad, fakeAudio("fixed"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(bad, later, later); err != nil {
		t.Fatal(err)
	}
	if res, err := processFile(pool, []string{music}, bad, covers, nil); res != resultInserted || err != nil {
		t.Fatalf("after the fix: %v %v", res, err)
	}
	if got := failureRows(t, pool); len(got) != 0 {
		t.Fatalf("failure kept after the file read: %v", got)
	}
}

// Content that is no audio is not indexed whatever its name, audio under the
// wrong extension is indexed as what it is, and a failure whose file is gone
// is forgotten by the next scan (#219).
func TestScanChecksContent(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, covers := t.TempDir(), t.TempDir()
	text := filepath.Join(music, "notes.mp3")
	flac := filepath.Join(music, "really-flac.mp3")
	opus := filepath.Join(music, "voice.ogg")
	if err := os.WriteFile(text, []byte("shopping list\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flac, flacWithComments("TITLE=F"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opus, oggFirstPage("OpusHead\x01"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	stats, err := Scan(pool, []string{music}, covers, nil, &p)
	if err != nil || stats.Inserted != 2 || stats.Failed != 1 {
		t.Fatalf("scan: %+v %v", stats, err)
	}
	got := songPaths(t, dbPath)
	if got[text] {
		t.Error("a text file named .mp3 was indexed")
	}
	if !strings.Contains(failureRows(t, pool)[text], "not an audio file") {
		t.Errorf("text file not recorded: %v", failureRows(t, pool))
	}
	for path, want := range map[string]string{flac: "flac audio/flac", opus: "opus audio/opus"} {
		var suffix, ctype string
		if err := pool.QueryRow("SELECT suffix, content_type FROM songs WHERE path = ?", path).Scan(&suffix, &ctype); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if suffix+" "+ctype != want {
			t.Errorf("%s: %s %s, want %s", filepath.Base(path), suffix, ctype, want)
		}
	}

	if err := os.Remove(text); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	if got := failureRows(t, pool); len(got) != 0 {
		t.Fatalf("failure of a removed file kept: %v", got)
	}
}

// The tag reader stops at maxTagBytes, so a file cannot make it read and
// hold an arbitrary amount; seeks stay free (#219).
func TestTagReadIsBounded(t *testing.T) {
	big := bytes.Repeat([]byte{0}, 100)
	l := &limitedReadSeeker{rs: bytes.NewReader(big), left: 40}
	b, err := io.ReadAll(l)
	if len(b) != 40 || !errors.Is(err, errTagTooLarge) {
		t.Fatalf("read %d bytes, err %v", len(b), err)
	}
	if off, err := l.Seek(-10, io.SeekEnd); err != nil || off != 90 {
		t.Fatalf("seek: %d %v", off, err)
	}
	// A real file whose ID3v2 tag claims more than the limit is indexed
	// with names from its path.
	defer func(orig int64) { maxTagBytes = orig }(maxTagBytes)
	maxTagBytes = 64 << 10
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, covers := t.TempDir(), t.TempDir()
	song := filepath.Join(music, "Huge Tag.mp3")
	hdr := []byte("ID3\x03\x00\x00\x7f\x7f\x7f\x7f") // syncsafe size ~256 MiB
	f, err := os.Create(song)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(hdr)
	if err := f.Truncate(maxTagBytes + 1<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if res, err := processFile(pool, []string{music}, song, covers, nil); err != nil || res != resultInserted {
		t.Fatalf("processFile: %v %v", res, err)
	}
	var title string
	if err := pool.QueryRow("SELECT title FROM songs WHERE path = ?", song).Scan(&title); err != nil || title != "Huge Tag" {
		t.Fatalf("title %q %v", title, err)
	}
}
