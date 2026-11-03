package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// library_modified changes when a scan inserts or removes songs, and stays
// put when a scan finds nothing new.
func TestLibraryModified(t *testing.T) {
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
	get := func() string {
		var v string
		_ = pool.QueryRow("SELECT value FROM settings WHERE key = ?", LibraryModifiedKey).Scan(&v)
		return v
	}
	var p Progress
	if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	if get() == "" {
		t.Fatal("insert did not record a change")
	}

	const sentinel = "2000-01-01T00:00:00Z"
	if _, err := pool.Exec("UPDATE settings SET value = ? WHERE key = ?", sentinel, LibraryModifiedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	if get() != sentinel {
		t.Fatalf("no-op scan changed library_modified to %q", get())
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(pool, []string{music}, t.TempDir(), nil, &p); err != nil {
		t.Fatal(err)
	}
	if v := get(); v == sentinel || v == "" {
		t.Fatalf("removal did not record a change: %q", v)
	}
}
