package scanner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestContainedRejectsSymlinkOutsideMusicFolder(t *testing.T) {
	music := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "passwd")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(music, "evil.mp3")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if Contained(music, link) {
		t.Fatal("accepted a link outside the music folder")
	}
	if _, err := OpenContained(music, link); !errors.Is(err, ErrOutsideMusic) {
		t.Fatalf("OpenContained error = %v, want ErrOutsideMusic", err)
	}
}

func TestOpenContainedAllowsInsideLinks(t *testing.T) {
	music := t.TempDir()
	song := filepath.Join(music, "song.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(music, "link.mp3")
	if err := os.Symlink("song.mp3", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f, err := OpenContained(music, link)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "audio" {
		t.Fatalf("got %q, want audio", b)
	}
}
