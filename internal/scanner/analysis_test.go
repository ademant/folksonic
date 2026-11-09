package scanner

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// The rescan rule of #71: tag values are stored; tool results survive a
// re-read while the file's size and mtime are unchanged and are dropped when
// the file changed, so the tools run again; a removed song takes its rows
// with it.
func TestScanKeepsAnalysisOfUnchangedAudio(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music := t.TempDir()
	f := filepath.Join(music, "tune.flac")
	if err := os.WriteFile(f, flacWithComments("TITLE=T", "BPM=128", "INITIALKEY=Am"), 0o644); err != nil {
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
	value := func(name string) string {
		t.Helper()
		var v sql.NullString
		err := pool.QueryRow("SELECT value FROM song_values WHERE name = ?", name).Scan(&v)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return v.String
	}
	tools := func() map[string]string {
		t.Helper()
		rows, err := pool.Query("SELECT feature, tool FROM analysis")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var f, tool string
			if err := rows.Scan(&f, &tool); err != nil {
				t.Fatal(err)
			}
			m[f] = tool
		}
		return m
	}
	bpm := func() (float64, string) {
		t.Helper()
		var b float64
		var src string
		if err := pool.QueryRow("SELECT bpm, bpm_source FROM songs").Scan(&b, &src); err != nil {
			t.Fatal(err)
		}
		return b, src
	}

	scan()
	tagKey := `{"tonic":"A","mode":"minor","confidence":null}`
	if v := value("key"); v != tagKey {
		t.Fatalf("key from tag: %q", v)
	}
	if m := tools(); len(m) != 1 || m["key"] != "tag" {
		t.Fatalf("analysis after first scan: %v", m)
	}
	var size, mtimeMs sql.NullInt64
	if err := pool.QueryRow("SELECT file_size, file_mtime_ms FROM songs").Scan(&size, &mtimeMs); err != nil || !size.Valid || !mtimeMs.Valid {
		t.Fatalf("file fingerprint not stored: %v %v %v", size, mtimeMs, err)
	}

	// What a folksentia run would have stored.
	if _, err := pool.Exec(`UPDATE songs SET bpm = 124, bpm_source = 'folksentia', bpm_confidence = 0.9;
		UPDATE analysis SET tool = 'folksentia' WHERE feature = 'key';
		UPDATE song_values SET value = '{"tonic":"C","mode":"major","confidence":0.8}' WHERE name = 'key';
		INSERT INTO analysis (song_id, feature, tool, analysed_ms) SELECT id, 'loudness', 'folksentia', 1 FROM songs;
		INSERT INTO song_values (song_id, name, value, changed_ms) SELECT id, 'loudness', '{"integratedLufs":-9}', 1 FROM songs`); err != nil {
		t.Fatal(err)
	}

	// A forced re-read (as migrations do with mtime = 0) of the same file.
	if _, err := pool.Exec("UPDATE songs SET mtime = 0"); err != nil {
		t.Fatal(err)
	}
	scan()
	if b, src := bpm(); b != 124 || src != "folksentia" {
		t.Fatalf("tool BPM replaced on re-read of unchanged audio: %v %s", b, src)
	}
	if v := value("key"); v != `{"tonic":"C","mode":"major","confidence":0.8}` {
		t.Fatalf("tool key replaced by tag: %q", v)
	}
	if m := tools(); len(m) != 2 || m["key"] != "folksentia" || m["loudness"] != "folksentia" {
		t.Fatalf("analysis after re-read: %v", m)
	}

	// The file changed: results are dropped and tag values come back. A track
	// that gave up on the old audio starts afresh with the new one.
	if _, err := pool.Exec(`INSERT INTO analysis_attempts (song_id, tool, attempts, started_ms) SELECT id, 'folksentia', 2, 1 FROM songs`); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(time.Hour)
	if err := os.Chtimes(f, ts, ts); err != nil {
		t.Fatal(err)
	}
	scan()
	var attempts int
	if err := pool.QueryRow("SELECT COUNT(*) FROM analysis_attempts").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("interrupted-attempt counts after the file changed: %d %v", attempts, err)
	}
	if b, src := bpm(); b != 128 || src != "tag" {
		t.Fatalf("BPM after the file changed: %v %s", b, src)
	}
	if v := value("key"); v != tagKey {
		t.Fatalf("key after the file changed: %q", v)
	}
	if v := value("loudness"); v != "" {
		t.Fatalf("stale loudness kept: %q", v)
	}
	if m := tools(); len(m) != 1 || m["key"] != "tag" {
		t.Fatalf("analysis after the file changed: %v", m)
	}

	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	scan()
	var n int
	if err := pool.QueryRow("SELECT (SELECT COUNT(*) FROM analysis) + (SELECT COUNT(*) FROM song_values)").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows of a removed song left: %d %v", n, err)
	}
}

func TestScanCallsAfterChange(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	called := make(chan bool, 1)
	p.SetAfterChange(func() { called <- p.Running() })
	if _, err := Scan(pool, []string{t.TempDir()}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-called:
		if running {
			t.Fatal("hook ran before the scan slot was released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook not called after the scan")
	}
}
