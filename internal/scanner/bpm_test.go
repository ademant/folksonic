package scanner

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// BPM from a tag is stored as 'tag'. A calculated value survives a re-read
// while the audio length is unchanged, and is cleared when it changed.
func TestScanBPMSources(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music := t.TempDir()
	tagged := filepath.Join(music, "tagged.flac")
	plain := filepath.Join(music, "plain.flac")
	if err := os.WriteFile(tagged, flacWithComments("TITLE=T", "BPM=128", "METER=2/4", "REPLAYGAIN_TRACK_GAIN=-2 dB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, flacWithComments("TITLE=P"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	scan := func() {
		t.Helper()
		if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		bpm    sql.NullFloat64
		source sql.NullString
		meter  sql.NullString
		gain   sql.NullFloat64
	}
	get := func(path string) row {
		t.Helper()
		var r row
		if err := pool.QueryRow("SELECT bpm, bpm_source, meter, rg_track_gain FROM songs WHERE path = ?", path).
			Scan(&r.bpm, &r.source, &r.meter, &r.gain); err != nil {
			t.Fatal(err)
		}
		return r
	}
	touch := func(path string, d time.Duration) {
		t.Helper()
		ts := time.Now().Add(d)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	scan()
	if r := get(tagged); r.bpm.Float64 != 128 || r.source.String != "tag" || r.meter.String != "2/4" || r.gain.Float64 != -2 {
		t.Fatalf("tagged: %+v", r)
	}
	if r := get(plain); r.bpm.Valid || r.source.Valid {
		t.Fatalf("plain: %+v", r)
	}

	// A calculation stored a value; a forced re-read (mtime reset) keeps it.
	if _, err := pool.Exec("UPDATE songs SET bpm = 101.5, bpm_source = 'calc', bpm_confidence = 0.4 WHERE path = ?", plain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("UPDATE songs SET mtime = 0"); err != nil {
		t.Fatal(err)
	}
	scan()
	if r := get(plain); r.bpm.Float64 != 101.5 || r.source.String != "calc" {
		t.Fatalf("calc value lost on re-read: %+v", r)
	}

	// The recording changed (different length): the calculated value goes.
	if _, err := pool.Exec("UPDATE songs SET duration_ms = 999 WHERE path = ?", plain); err != nil {
		t.Fatal(err)
	}
	touch(plain, time.Hour)
	scan()
	if r := get(plain); r.bpm.Valid || r.source.Valid {
		t.Fatalf("calc value kept for a changed recording: %+v", r)
	}

	// Removing the tag removes the tag value.
	if err := os.WriteFile(tagged, flacWithComments("TITLE=T"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(tagged, 2*time.Hour)
	scan()
	if r := get(tagged); r.bpm.Valid || r.source.Valid || r.meter.Valid {
		t.Fatalf("tag removed but values kept: %+v", r)
	}
}
