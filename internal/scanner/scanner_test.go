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
	if _, err := Scan(pool, music, covers, &p); err != nil {
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
	stats, err := Scan(pool, music, covers, &p)
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
	if _, err := Scan(pool, music, t.TempDir(), &p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)

	stats, err := Scan(pool, music, t.TempDir(), &p)
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
