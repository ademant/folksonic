package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A songs table from before created_at existed gets the column, filled from
// each file's mtime, and the FTS update trigger is narrowed so the backfill
// (and later bookkeeping updates) do not rewrite the search index.
func TestMigrateSongCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	pool, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	// Turn the fresh database back into the old layout.
	for _, q := range []string{
		"DROP TRIGGER songs_au",
		`CREATE TRIGGER songs_au AFTER UPDATE ON songs BEGIN
		    DELETE FROM songs_fts WHERE id = OLD.id;
		    INSERT INTO songs_fts (id, title, artist, album, genre)
		    VALUES (NEW.id, NEW.title, NEW.artist, NEW.album, COALESCE(NEW.genre, ''));
		 END`,
		"ALTER TABLE songs DROP COLUMN created_at",
		`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')`,
		`INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')`,
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime)
		 VALUES ('so-1', '/m/1.mp3', 'One', 'A', 'ar-1', 'B', 'al-1', 'mp3', 'audio/mpeg', 1700000000),
		        ('so-2', '/m/2.mp3', 'Two', 'A', 'ar-1', 'B', 'al-1', 'mp3', 'audio/mpeg', 0)`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ftsRowid := func() int64 {
		var id int64
		if err := pool.QueryRow("SELECT rowid FROM songs_fts WHERE id = 'so-1'").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := ftsRowid()

	if err := migrateSongCreated(pool); err != nil {
		t.Fatal(err)
	}
	var c1, c2 sql.NullString
	if err := pool.QueryRow("SELECT (SELECT created_at FROM songs WHERE id='so-1'), (SELECT created_at FROM songs WHERE id='so-2')").Scan(&c1, &c2); err != nil {
		t.Fatal(err)
	}
	if c1.String != "2023-11-14T22:13:20Z" {
		t.Fatalf("created_at from mtime = %q", c1.String)
	}
	if !c2.Valid || strings.HasPrefix(c2.String, "1970") {
		t.Fatalf("mtime 0 must not give 1970: %v", c2)
	}
	if after := ftsRowid(); after != before {
		t.Fatalf("backfill rewrote the FTS row (rowid %d -> %d)", before, after)
	}

	// Bookkeeping updates leave the index alone; title changes still reach it.
	if _, err := pool.Exec("UPDATE songs SET mtime = 1 WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	if after := ftsRowid(); after != before {
		t.Fatal("mtime update rewrote the FTS row")
	}
	if _, err := pool.Exec("UPDATE songs SET title = 'Uno' WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM songs_fts WHERE songs_fts MATCH 'uno'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("title change not indexed: %d, %v", n, err)
	}

	// Idempotent, and keeps the values.
	if err := migrate(pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var again string
	if err := pool.QueryRow("SELECT created_at FROM songs WHERE id='so-1'").Scan(&again); err != nil || again != c1.String {
		t.Fatalf("second migrate changed created_at: %q, %v", again, err)
	}
	pool.Close()
}

// An existing songs table gets the loudness, meter and BPM columns and every
// song is marked for one re-read, atomically; a second run changes nothing.
func TestMigrateAudioColumns(t *testing.T) {
	pool, err := Init(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, c := range audioColumns {
		if _, err := pool.Exec("ALTER TABLE songs DROP COLUMN " + c.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1','A','a');
		INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1','B','A','ar-1');
		INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime)
		VALUES ('so-1','/m/1.opus','One','A','ar-1','B','al-1','opus','audio/ogg',1700000000)`); err != nil {
		t.Fatal(err)
	}
	if err := migrateAudioColumns(pool); err != nil {
		t.Fatal(err)
	}
	var mtime int64
	if err := pool.QueryRow("SELECT mtime FROM songs").Scan(&mtime); err != nil || mtime != 0 {
		t.Fatalf("song not marked for re-read: mtime %d, %v", mtime, err)
	}
	for _, c := range audioColumns {
		if found, _, _ := tableColumn(pool, "songs", c.name); !found {
			t.Fatalf("column %s missing", c.name)
		}
	}
	if _, err := pool.Exec("UPDATE songs SET mtime = 5"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow("SELECT mtime FROM songs").Scan(&mtime); err != nil || mtime != 5 {
		t.Fatalf("second migration reset mtime again: %d", mtime)
	}
}
