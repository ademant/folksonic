package db

import (
	"path/filepath"
	"testing"
)

// #165: the migration resets the mtime of songs without a duration only, so the
// next scan re-reads them and leaves the rest alone.
func TestMigrateOggDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')`,
		`INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'Al', 'A', 'ar-1')`,
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime, duration_ms)
		 VALUES ('so-0', '/m/0.ogg', 'Zero', 'A', 'ar-1', 'Al', 'al-1', 'ogg', 'audio/ogg', 42, 0)`,
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime, duration_ms)
		 VALUES ('so-1', '/m/1.ogg', 'One', 'A', 'ar-1', 'Al', 'al-1', 'ogg', 'audio/ogg', 42, 5000)`,
		// Back to the layout before the migration, so it runs again on the next Init.
		`PRAGMA user_version = 10`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	d, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mtimes := map[string]int64{}
	rows, err := d.Query(`SELECT id, mtime FROM songs`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var mtime int64
		if err := rows.Scan(&id, &mtime); err != nil {
			t.Fatal(err)
		}
		mtimes[id] = mtime
	}
	rows.Close()
	if mtimes["so-0"] != 0 {
		t.Fatalf("song without duration: mtime %d, want 0 (re-read)", mtimes["so-0"])
	}
	if mtimes["so-1"] != 42 {
		t.Fatalf("song with duration: mtime %d, want 42 (kept)", mtimes["so-1"])
	}
}
