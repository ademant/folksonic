package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

func newTestDB(t *testing.T) (dbPath string, close func()) {
	t.Helper()
	dir := t.TempDir()
	pool, err := db.Init(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "test.db"), func() { pool.Close() }
}

func countSongs(t *testing.T, path string) int {
	t.Helper()
	pool, err := db.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM songs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A deleted file must be pruned on the next scan, even though the watcher
// never saw the deletion (the server was "down").
func TestScanPrunesFilesDeletedWhileOffline(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music := t.TempDir()
	covers := t.TempDir()
	for _, name := range []string{"a.mp3", "b.mp3"} {
		if err := os.WriteFile(filepath.Join(music, name), []byte("not really audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var p Progress
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if n := countSongs(t, dbPath); n != 2 {
		t.Fatalf("after first scan: %d songs, want 2", n)
	}

	if err := os.Remove(filepath.Join(music, "a.mp3")); err != nil {
		t.Fatal(err)
	}
	pool, err = db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	stats, err := Scan(pool, []string{music}, covers, nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1", stats.Pruned)
	}
	var n int
	pool.QueryRow("SELECT COUNT(*) FROM songs").Scan(&n)
	if n != 1 {
		t.Fatalf("after prune: %d songs, want 1", n)
	}
}

// An unreadable directory must not cause its songs to be pruned.
func TestScanKeepsSongsInUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 000 directories")
	}
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music := t.TempDir()
	sub := filepath.Join(music, "locked")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var p Progress
	if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)

	stats, err := Scan(pool, []string{music}, t.TempDir(), nil, &p)
	pool.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 0 {
		t.Fatalf("Pruned = %d, want 0 while a directory is unreadable", stats.Pruned)
	}
	if n := countSongs(t, dbPath); n != 1 {
		t.Fatalf("song removed from unreadable directory: %d songs", n)
	}
}

func writeAudio(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Songs in nested subdirectories of several roots are all indexed, and a
// folder reached through two overlapping roots is indexed once.
func TestScanMultipleRootsWithSubdirs(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	a, b := t.TempDir(), t.TempDir()
	writeAudio(t, filepath.Join(a, "x", "y", "deep.mp3"))
	writeAudio(t, filepath.Join(b, "album", "track.flac"))
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	// b is also listed as a child of a, so its files are reached twice.
	stats, err := Scan(pool, []string{a, b, filepath.Join(a, "x")}, t.TempDir(), nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1+1 {
		// deep.mp3 is under a/x (once); track.flac under b (once).
		t.Fatalf("Inserted = %d, want 2 (each file once)", stats.Inserted)
	}
}

// A root that cannot be read must not prune songs of the other roots, and
// must not prune its own songs either.
func TestUnavailableRootOnlyBlocksItsOwnPruning(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	good := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unmounted")
	writeAudio(t, filepath.Join(good, "keep.mp3"))
	writeAudio(t, filepath.Join(good, "remove.mp3"))
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	if _, err := Scan(pool, []string{good}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	// Seed a song under the unavailable root, as if it had been scanned earlier.
	if _, err := pool.Exec(`INSERT INTO artists (id, name, name_lower) VALUES ('ar-t','T','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-t','T','T','ar-t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type)
		VALUES ('so-t', ?, 'T', 'T', 'ar-t', 'T', 'al-t', 'mp3', 'audio/mpeg')`, filepath.Join(gone, "lost.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(good, "remove.mp3")); err != nil {
		t.Fatal(err)
	}

	stats, err := Scan(pool, []string{good, gone}, t.TempDir(), nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1 (only remove.mp3 under the readable root)", stats.Pruned)
	}
	var n int
	pool.QueryRow("SELECT COUNT(*) FROM songs WHERE id = 'so-t'").Scan(&n)
	if n != 1 {
		t.Fatal("song under the unavailable root was pruned")
	}
}
