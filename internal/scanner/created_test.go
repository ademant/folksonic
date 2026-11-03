package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// created_at is set when a file is first added and kept when a changed file
// is re-read by a later scan.
func TestCreatedSetOnInsertAndKeptOnRescan(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music := t.TempDir()
	file := filepath.Join(music, "a.mp3")
	writeAudio(t, file)
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	before := time.Now().UTC().Add(-time.Second).Format("2006-01-02T15:04:05Z")
	if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	var created string
	if err := pool.QueryRow("SELECT created_at FROM songs").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created < before {
		t.Fatalf("created_at = %q, want the scan time (>= %s)", created, before)
	}

	// Pretend the song was added long ago, then change the file.
	if _, err := pool.Exec("UPDATE songs SET created_at = '2001-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	stats, err := Scan(pool, []string{music}, t.TempDir(), nil, &p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Updated != 1 {
		t.Fatalf("Updated = %d, want 1 (file changed)", stats.Updated)
	}
	if err := pool.QueryRow("SELECT created_at FROM songs").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != "2001-01-01T00:00:00Z" {
		t.Fatalf("rescan changed created_at to %q", created)
	}
}
