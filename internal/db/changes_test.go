package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// dropChangeTriggers removes the change-feed triggers. A database from before
// the change feed has none, so tests that rebuild such a database by dropping
// columns must remove them first: SQLite refuses to drop a column that a
// trigger refers to.
func dropChangeTriggers(t *testing.T, d *sql.DB) {
	t.Helper()
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'chg\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		if _, err := d.Exec("DROP TRIGGER " + n); err != nil {
			t.Fatal(err)
		}
	}
}

func stampOf(t *testing.T, d *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := d.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// tick makes sure the next write gets a later millisecond timestamp.
func tick() { time.Sleep(3 * time.Millisecond) }

func seedChangeLibrary(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO users (id, username, password_hash, role, status, created_at) VALUES (1, 'u', 'x', 'user', 'active', 't')",
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-1', '/m/x.flac', 'x', 'A', 'ar-1', 'B', 'al-1', 'flac', 'audio/flac', 1)",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestChangeFeedStampsRowsOnInsert(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	seedChangeLibrary(t, d)
	for _, q := range []string{
		"SELECT updated_ms FROM artists WHERE id = 'ar-1'",
		"SELECT updated_ms FROM albums WHERE id = 'al-1'",
		"SELECT updated_ms FROM songs WHERE id = 'so-1'",
	} {
		if v := stampOf(t, d, q); v <= 0 {
			t.Fatalf("%s = %d, want a timestamp", q, v)
		}
	}
}

// A tag change bumps the song, its album and its artist. A write that leaves
// every tracked value as it was bumps nothing.
func TestChangeFeedTagChangeBumpsParents(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	seedChangeLibrary(t, d)
	artist0 := stampOf(t, d, "SELECT updated_ms FROM artists WHERE id = 'ar-1'")
	album0 := stampOf(t, d, "SELECT updated_ms FROM albums WHERE id = 'al-1'")
	song0 := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'")

	tick()
	if _, err := d.Exec("UPDATE songs SET title = 'y' WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	if v := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'"); v <= song0 {
		t.Errorf("song not bumped by a tag change: %d <= %d", v, song0)
	}
	if v := stampOf(t, d, "SELECT updated_ms FROM albums WHERE id = 'al-1'"); v <= album0 {
		t.Errorf("album not bumped by a song's tag change: %d <= %d", v, album0)
	}
	if v := stampOf(t, d, "SELECT updated_ms FROM artists WHERE id = 'ar-1'"); v <= artist0 {
		t.Errorf("artist not bumped by a song's tag change: %d <= %d", v, artist0)
	}

	// Rescans rewrite every column, most with the same value. Nothing changed,
	// so nothing is bumped; the untracked created_at column is left alone too.
	still := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'")
	tick()
	if _, err := d.Exec("UPDATE songs SET title = title, mtime = mtime, created_at = 'later' WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	if v := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'"); v != still {
		t.Errorf("unchanged write bumped the song: %d != %d", v, still)
	}
}

func TestChangeFeedPlaylistEntriesBumpPlaylist(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	seedChangeLibrary(t, d)
	if _, err := d.Exec("INSERT INTO playlists (id, name, created_at, updated_at, owner_id) VALUES ('pl-1', 'P', 't', 't', 1)"); err != nil {
		t.Fatal(err)
	}
	at := func() int64 { return stampOf(t, d, "SELECT updated_ms FROM playlists WHERE id = 'pl-1'") }
	created := at()
	if created <= 0 {
		t.Fatal("new playlist not stamped")
	}

	tick()
	if _, err := d.Exec("INSERT INTO playlist_songs (playlist_id, position, song_id) VALUES ('pl-1', 0, 'so-1')"); err != nil {
		t.Fatal(err)
	}
	added := at()
	if added <= created {
		t.Errorf("adding a song did not bump the playlist: %d <= %d", added, created)
	}

	tick()
	if _, err := d.Exec("DELETE FROM playlist_songs WHERE playlist_id = 'pl-1'"); err != nil {
		t.Fatal(err)
	}
	if removed := at(); removed <= added {
		t.Errorf("removing songs did not bump the playlist: %d <= %d", removed, added)
	}

	tick()
	if _, err := d.Exec("UPDATE playlists SET name = 'Q' WHERE id = 'pl-1'"); err != nil {
		t.Fatal(err)
	}
	if renamed := at(); renamed <= created {
		t.Errorf("rename did not bump the playlist")
	}
}

func TestChangeFeedTombstones(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	seedChangeLibrary(t, d)
	if _, err := d.Exec("INSERT INTO playlists (id, name, created_at, updated_at, owner_id) VALUES ('pl-1', 'P', 't', 't', 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO starred (user_id, id, starred_at) VALUES (1, 'so-1', 't')"); err != nil {
		t.Fatal(err)
	}

	// Removing the only song removes its album and artist too, and writes a
	// tombstone for each. A song's tombstone is for everyone.
	for _, q := range []string{
		"DELETE FROM starred WHERE id = 'so-1'",
		"DELETE FROM songs WHERE id = 'so-1'",
		"DELETE FROM albums WHERE id = 'al-1'",
		"DELETE FROM artists WHERE id = 'ar-1'",
		"DELETE FROM playlists WHERE id = 'pl-1'",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	type tomb struct {
		kind, id string
		user     sql.NullInt64
	}
	rows, err := d.Query("SELECT kind, item_id, user_id FROM change_tombstones ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []tomb
	for rows.Next() {
		var x tomb
		if err := rows.Scan(&x.kind, &x.id, &x.user); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	want := []tomb{
		{"starred", "so-1", sql.NullInt64{Int64: 1, Valid: true}},
		{"song", "so-1", sql.NullInt64{}},
		{"album", "al-1", sql.NullInt64{}},
		{"artist", "ar-1", sql.NullInt64{}},
		{"playlist", "pl-1", sql.NullInt64{Int64: 1, Valid: true}},
	}
	if len(got) != len(want) {
		t.Fatalf("tombstones %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tombstone %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPruneChangeTombstonesRaisesFloor(t *testing.T) {
	ctx := context.Background()
	d, err := Init(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec("INSERT INTO change_tombstones (kind, item_id, user_id, deleted_ms) VALUES ('song', 'so-old', NULL, 1000), ('song', 'so-new', NULL, ?)",
		time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := PruneChangeTombstones(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	if n := stampOf(t, d, "SELECT COUNT(*) FROM change_tombstones"); n != 1 {
		t.Fatalf("%d tombstones after prune, want the recent one only", n)
	}
	floor, err := ChangeFloor(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh database's floor is already the time of its ID reset, which is
	// later than the retention cutoff, so the test checks a lower bound only.
	if want := now.Add(-changeRetention).UnixMilli(); floor < want {
		t.Fatalf("floor %d is before the retention cutoff %d", floor, want)
	}

	// The floor never moves backwards.
	if err := PruneChangeTombstones(ctx, d, now.Add(-time.Hour*24*365)); err != nil {
		t.Fatal(err)
	}
	again, _ := ChangeFloor(ctx, d)
	if again < floor {
		t.Fatalf("floor moved back from %d to %d", floor, again)
	}
}

// A database from before the change feed gets updated_ms, backfilled, and the
// triggers, on the next start. A second start changes nothing.
func TestChangeFeedMigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	seedChangeLibrary(t, d)
	dropChangeTriggers(t, d)
	if _, err := d.Exec("ALTER TABLE songs DROP COLUMN updated_ms"); err != nil {
		t.Fatal(err)
	}
	// A database from before schema versions: the probe steps must run again.
	if _, err := d.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'")
	if first <= 0 {
		t.Fatalf("backfill left updated_ms = %d", first)
	}
	tick()
	if _, err := d.Exec("UPDATE songs SET title = 'z' WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	bumped := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'")
	if bumped <= first {
		t.Fatal("triggers not installed by the migration")
	}
	d.Close()

	d, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := stampOf(t, d, "SELECT updated_ms FROM songs WHERE id = 'so-1'"); got != bumped {
		t.Fatalf("second start changed the stamp: %d, want %d", got, bumped)
	}
}
