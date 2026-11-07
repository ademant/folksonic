package db

import (
	"path/filepath"
	"testing"
)

// A database from before #71 gains the analysis tables and the file columns
// without losing values or forcing a re-read; running it again changes
// nothing; analysis rows follow their song.
func TestMigrateAnalysis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')`,
		`INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'Al', 'A', 'ar-1')`,
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime, bpm, bpm_source)
		 VALUES ('so-1', '/m/1.flac', 'T', 'A', 'ar-1', 'Al', 'al-1', 'flac', 'audio/flac', 42, 120, 'calc')`,
		// Back to the layout before the migration.
		`DROP TABLE analysis`, `DROP TABLE song_values`,
		`ALTER TABLE songs DROP COLUMN file_size`, `ALTER TABLE songs DROP COLUMN file_mtime_ms`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	for range 2 {
		d, err = Init(path)
		if err != nil {
			t.Fatal(err)
		}
		var mtime int64
		var bpm float64
		var size, mtimeMs any
		if err := d.QueryRow("SELECT mtime, bpm, file_size, file_mtime_ms FROM songs").Scan(&mtime, &bpm, &size, &mtimeMs); err != nil {
			t.Fatal(err)
		}
		if mtime != 42 || bpm != 120 || size != nil || mtimeMs != nil {
			t.Fatalf("after migration: mtime %d bpm %v size %v mtime_ms %v", mtime, bpm, size, mtimeMs)
		}
		d.Close()
	}

	d, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO analysis (song_id, feature, tool, analysed_ms) VALUES ('so-1', 'key', 'tag', 1);
		INSERT INTO song_values (song_id, name, value, changed_ms) VALUES ('so-1', 'key', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO analysis (song_id, feature, tool, analysed_ms) VALUES ('so-none', 'key', 'tag', 1)`); err == nil {
		t.Fatal("analysis row for a missing song accepted")
	}
	if _, err := d.Exec(`UPDATE songs SET id = 'so-2'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT (SELECT COUNT(*) FROM analysis WHERE song_id = 'so-2') + (SELECT COUNT(*) FROM song_values WHERE song_id = 'so-2')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows did not follow the new id: %d %v", n, err)
	}
	if _, err := d.Exec(`DELETE FROM songs`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT (SELECT COUNT(*) FROM analysis) + (SELECT COUNT(*) FROM song_values)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows of a deleted song left: %d %v", n, err)
	}
}
