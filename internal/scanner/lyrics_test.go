package scanner

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// seededDB opens a fresh database with one song, so-1, and its artist and album.
func seededDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Init(filepath.Join(t.TempDir(), "lyrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	for _, q := range []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'a', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'b', 'a', 'ar-1')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) " +
			"VALUES ('so-1', '/m/1.mp3', 't', 'a', 'ar-1', 'b', 'al-1', 'mp3', 'audio/mpeg', 1)",
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// #142: a lyrics tag becomes the stored text; a blank tag is no lyrics. Synced
// text is kept with its timestamps.
func TestLyricsValue(t *testing.T) {
	if got := lyricsValue("  \n"); got.Valid {
		t.Fatalf("blank lyrics valid: %q", got.String)
	}
	synced := "[00:01.00]Hello\n[00:03.00]World"
	if got := lyricsValue(synced); !got.Valid || got.String != synced {
		t.Fatalf("synced text changed: %+v", got)
	}
}

// storeLyrics writes, replaces and removes a song's lyrics row.
func TestStoreLyricsReplacesAndRemoves(t *testing.T) {
	pool := seededDB(t)
	ctx := context.Background()
	write := func(v sql.NullString) {
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := storeLyrics(tx, "so-1", v); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		if err := pool.QueryRow("SELECT COUNT(*) FROM song_lyrics").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	write(sql.NullString{String: "first", Valid: true})
	write(sql.NullString{String: "second", Valid: true})
	var text string
	if err := pool.QueryRow("SELECT text FROM song_lyrics WHERE song_id = 'so-1'").Scan(&text); err != nil || text != "second" || count() != 1 {
		t.Fatalf("after replace: text %q, rows %d, err %v", text, count(), err)
	}
	write(sql.NullString{})
	if count() != 0 {
		t.Fatal("lyrics row kept after the tag was removed")
	}
}

// Deleting a song takes its lyrics with it.
func TestLyricsGoWithTheSong(t *testing.T) {
	pool := seededDB(t)
	if _, err := pool.Exec("INSERT INTO song_lyrics (song_id, text) VALUES ('so-1', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("DELETE FROM songs WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM song_lyrics").Scan(&n); err != nil || n != 0 {
		t.Fatalf("lyrics left after the song: %d, %v", n, err)
	}
}
