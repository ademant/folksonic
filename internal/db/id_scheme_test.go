package db

import (
	"path/filepath"
	"testing"
)

// An old database has library rows with MD5-derived IDs and no id_scheme
// setting. The migration clears the library and keeps playlists, once.
func TestIDSchemeResetsLibraryOnce(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "ids.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for _, q := range []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-old', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-old', 'B', 'A', 'ar-old')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-old', '/m/x.flac', 'x', 'A', 'ar-old', 'B', 'al-old', 'flac', 'audio/flac', 1)",
		"INSERT INTO playlists (id, name, created_at, updated_at) VALUES ('pl-keep', 'Keep', 't', 't')",
		"INSERT INTO playlist_songs (playlist_id, position, song_id) VALUES ('pl-keep', 1, 'so-old')",
		"INSERT INTO starred (id, starred_at) VALUES ('so-old', 't')",
		"DELETE FROM settings WHERE key = 'id_scheme'",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if err := migrateIDScheme(d); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := d.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT COUNT(*) FROM songs"); n != 0 {
		t.Fatalf("songs not cleared: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM songs_fts"); n != 0 {
		t.Fatalf("search index not cleared: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM playlist_songs"); n != 0 {
		t.Fatalf("playlist entries pointing at removed songs kept: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM starred"); n != 0 {
		t.Fatalf("starred items pointing at removed songs kept: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM playlists WHERE id = 'pl-keep'"); n != 1 {
		t.Fatalf("playlist itself was removed: %d", n)
	}

	// A song added after the reset survives a second run: the migration is once only.
	for _, q := range []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-new', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-new', 'B', 'A', 'ar-new')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-new', '/m/y.flac', 'y', 'A', 'ar-new', 'B', 'al-new', 'flac', 'audio/flac', 1)",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateIDScheme(d); err != nil {
		t.Fatal(err)
	}
	if n := count("SELECT COUNT(*) FROM songs"); n != 1 {
		t.Fatalf("second run cleared the library: %d songs", n)
	}
}
