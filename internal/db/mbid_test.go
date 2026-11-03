package db

import (
	"path/filepath"
	"testing"
)

// A database from before MusicBrainz IDs: the songs table has no mbid column.
// Opening it must add the column and reset mtimes, so the next scan re-reads
// the tags and fills the IDs in.
func TestMigrationAddsMBIDAndForcesRescan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mbid.db")
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-1', '/m/x.flac', 'x', 'A', 'ar-1', 'B', 'al-1', 'flac', 'audio/flac', 12345)",
		"ALTER TABLE songs DROP COLUMN mbid",
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	d, err = Init(path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer d.Close()
	var mtime int64
	if err := d.QueryRow("SELECT mtime FROM songs WHERE id = 'so-1'").Scan(&mtime); err != nil || mtime != 0 {
		t.Fatalf("mtime not reset for rescan: %v %d", err, mtime)
	}
	if _, err := d.Exec("UPDATE songs SET mbid = 'x' WHERE id = 'so-1'"); err != nil {
		t.Fatalf("mbid column missing after migration: %v", err)
	}
}
