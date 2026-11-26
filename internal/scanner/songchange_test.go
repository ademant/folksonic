package scanner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// A scan reports each song it adds, re-reads or prunes, so the server can
// drop its cached path (#197).
func TestScanReportsSongChanges(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, covers := t.TempDir(), t.TempDir()
	song := filepath.Join(music, "a.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var p Progress
	var got []string
	p.SetSongChanged(func(id string) { got = append(got, id) })
	id := SongID(song)

	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{id}) {
		t.Fatalf("insert reported %v, want [%s]", got, id)
	}
	got = nil
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an unchanged song was reported: %v", got)
	}
	if err := os.Remove(song); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{id}) {
		t.Fatalf("prune reported %v, want [%s]", got, id)
	}

	// The watcher's delete path reports the id too, and nothing for a path
	// without a song.
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := deletePrefix(pool, music, &p); err != nil {
		t.Fatal(err)
	}
	if removed, err := DeletePath(pool, song); err != nil || removed != "" {
		t.Fatalf("second delete: %q %v", removed, err)
	}
	if !slices.Equal(got, []string{id}) {
		t.Fatalf("watcher delete reported %v, want [%s]", got, id)
	}
}
