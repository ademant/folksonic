package scanner

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhowden/tag"
	"github.com/tsirysndr/tinysonic/internal/db"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

// A folder cover is copied only when it is an image, named for its real
// format, and only up to maxCoverBytes (#198).
func TestFolderCoverMustBeAnImage(t *testing.T) {
	music, covers := t.TempDir(), t.TempDir()
	song := filepath.Join(music, "a.mp3")
	if err := os.WriteFile(song, fakeAudio("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []string{music}
	cover := filepath.Join(music, "cover.jpg")

	if err := os.WriteFile(cover, []byte("not an image at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if name, ok := findDirCover(roots, song, covers, "al-1"); ok {
		t.Fatalf("copied a cover that is not an image: %s", name)
	}

	// A PNG named cover.jpg is stored as .png.
	if err := os.WriteFile(cover, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	name, ok := findDirCover(roots, song, covers, "al-1")
	if !ok || name != "al-1.png" {
		t.Fatalf("PNG cover: %q %v", name, ok)
	}
	if b, _ := os.ReadFile(filepath.Join(covers, name)); !bytes.Equal(b, pngBytes) {
		t.Fatalf("copied %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(covers, ".cover-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// Too large.
	big := append(append([]byte{}, pngBytes...), make([]byte, maxCoverBytes)...)
	if err := os.WriteFile(cover, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if name, ok := findDirCover(roots, song, t.TempDir(), "al-2"); ok {
		t.Fatalf("copied a cover over the limit: %s", name)
	}
}

// An embedded picture is checked the same way.
func TestEmbeddedPictureBounds(t *testing.T) {
	covers := t.TempDir()
	if _, err := savePicture(covers, "al-1", &tag.Picture{Ext: "jpg", Data: []byte("<html>")}); !errors.Is(err, errNotImage) {
		t.Fatalf("non-image picture: %v", err)
	}
	big := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, maxCoverBytes)...)
	if _, err := savePicture(covers, "al-1", &tag.Picture{Ext: "jpg", Data: big}); !errors.Is(err, errNotImage) {
		t.Fatalf("oversized picture: %v", err)
	}
	name, err := savePicture(covers, "al-1", &tag.Picture{Ext: "jpg", MIMEType: "image/jpeg", Data: pngBytes})
	if err != nil || name != "al-1.png" {
		t.Fatalf("PNG claimed as JPEG: %q %v", name, err)
	}
}

func TestLyricsAreBounded(t *testing.T) {
	if v := lyricsValue("la la la"); !v.Valid {
		t.Fatal("short lyrics dropped")
	}
	if v := lyricsValue(strings.Repeat("x", maxLyricsBytes+1)); v.Valid {
		t.Fatal("oversized lyrics kept")
	}
}

// A same-size rewrite within the same second is read again: the stored
// millisecond mtime differs (#198).
func TestSameSecondRewriteIsNotSkipped(t *testing.T) {
	dbPath, closeDB := newTestDB(t)
	defer closeDB()
	music, covers := t.TempDir(), t.TempDir()
	song := filepath.Join(music, "a.mp3")
	if err := os.WriteFile(song, fakeAudio("audio-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 100*int64(time.Millisecond))
	if err := os.Chtimes(song, base, base); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Init(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	roots := []string{music}
	if _, err := processFile(pool, roots, song, covers, nil); err != nil {
		t.Fatal(err)
	}
	if res, err := processFile(pool, roots, song, covers, nil); err != nil || res != resultSkipped {
		t.Fatalf("unchanged file: %v %v", res, err)
	}
	if err := os.WriteFile(song, fakeAudio("audio-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := base.Add(500 * time.Millisecond) // same second, same size
	if err := os.Chtimes(song, later, later); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(song); st.ModTime().UnixMilli() != later.UnixMilli() {
		t.Skip("file system keeps no millisecond mtime")
	}
	if res, err := processFile(pool, roots, song, covers, nil); err != nil || res == resultSkipped {
		t.Fatalf("same-second rewrite skipped: %v %v", res, err)
	}
}
