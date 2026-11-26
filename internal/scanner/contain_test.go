package scanner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
}

func songPaths(t *testing.T, dbPath string) map[string]bool {
	t.Helper()
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query("SELECT path FROM songs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out[p] = true
	}
	return out
}

// A link that leads outside the music folder is not indexed, whether it is a
// file named like audio or a directory; links inside the folder still are
// (#177). A song indexed through such a link before is pruned.
func TestScanSkipsSymlinksOutsideTheMusicFolder(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, outside, covers := t.TempDir(), t.TempDir(), t.TempDir()

	secret := filepath.Join(outside, "passwd")
	if err := os.WriteFile(secret, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "album", "x.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(music, "real", "song.mp3")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-audio file outside, with an audio name inside the folder.
	mustSymlink(t, secret, filepath.Join(music, "evil.mp3"))
	// A directory outside.
	mustSymlink(t, filepath.Join(outside, "album"), filepath.Join(music, "escape"))
	// Links that stay inside: a file and a directory.
	mustSymlink(t, inside, filepath.Join(music, "alias.mp3"))
	mustSymlink(t, filepath.Join(music, "real"), filepath.Join(music, "real-link"))

	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var p Progress
	if _, err := Scan(pool, []string{music}, covers, nil, &p); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	got := songPaths(t, dbPath)
	for _, bad := range []string{filepath.Join(music, "evil.mp3"), filepath.Join(music, "escape", "x.mp3")} {
		if got[bad] {
			t.Errorf("indexed %s, which leads outside the music folder", bad)
		}
	}
	if !got[inside] || !got[filepath.Join(music, "alias.mp3")] {
		t.Errorf("links inside the folder were not indexed: %v", got)
	}

	// The watcher refuses the same link.
	pool, err = db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := processFile(pool, []string{music}, filepath.Join(music, "evil.mp3"), covers, nil); !errors.Is(err, ErrOutsideMusic) {
		t.Fatalf("processFile on a link outside: %v", err)
	}
}

// A song that an older version indexed through a link outside the folder is
// pruned by the next scan.
func TestScanPrunesSongsIndexedThroughAnEscapingLink(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, outside, covers := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "passwd")
	if err := os.WriteFile(secret, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(music, "evil.mp3")
	mustSymlink(t, secret, link)
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a');
		INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime)
		VALUES ('so-1', ?, 'x', 'A', 'ar-1', 'B', 'al-1', 'mp3', 'audio/mpeg', 1)`, link); err != nil {
		t.Fatal(err)
	}
	var p Progress
	stats, err := Scan(pool, []string{music}, covers, nil, &p)
	pool.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 1 || len(songPaths(t, dbPath)) != 0 {
		t.Fatalf("pruned %d, songs %v", stats.Pruned, songPaths(t, dbPath))
	}
}

// OpenContained refuses a link that leads outside, also one changed after
// the scan, and opens files and inside links.
func TestOpenContained(t *testing.T) {
	music, outside := t.TempDir(), t.TempDir()
	song := filepath.Join(music, "a.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "passwd")
	if err := os.WriteFile(secret, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(music, "b.mp3")
	mustSymlink(t, song, link) // absolute target, inside the folder
	rel := filepath.Join(music, "c.mp3")
	mustSymlink(t, "a.mp3", rel) // relative target
	roots := []string{music}

	for _, p := range []string{song, link, rel} {
		f, err := OpenContained(roots, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != "audio" {
			t.Fatalf("%s: read %q", p, b)
		}
	}
	// The link is pointed outside after the scan.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, secret, link)
	if f, err := OpenContained(roots, link); err == nil {
		f.Close()
		t.Fatal("opened a link that leads outside the music folder")
	}
	if Contained(roots, link) {
		t.Fatal("Contained accepted a link that leads outside")
	}
	// A path in no music folder at all.
	if _, err := OpenContained(roots, secret); !errors.Is(err, ErrOutsideMusic) {
		t.Fatalf("path outside every root: %v", err)
	}
	// A cover.jpg linking outside is not copied.
	mustSymlink(t, secret, filepath.Join(music, "cover.jpg"))
	covers := t.TempDir()
	if name, ok := findDirCover(roots, song, covers, "al-1"); ok {
		t.Fatalf("copied a cover that links outside: %s", name)
	}
}

// A music folder that is itself a symlink works: containment is checked
// against the folder's resolved path.
func TestSymlinkedRootIsAllowed(t *testing.T) {
	real := t.TempDir()
	song := filepath.Join(real, "a.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "music")
	mustSymlink(t, real, root)
	p := filepath.Join(root, "a.mp3")
	if !Contained([]string{root}, p) {
		t.Fatal("file under a symlinked root not contained")
	}
	f, err := OpenContained([]string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
