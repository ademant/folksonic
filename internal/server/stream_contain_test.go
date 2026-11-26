package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A song whose file is a link pointed outside the music folders after the
// scan is not served (#177): stream and download open through os.Root.
func TestStreamRefusesLinkOutsideTheMusicFolder(t *testing.T) {
	s, tok, _ := streamFixture(t)
	outside := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(outside, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.MusicDirs[0], "evil.mp3")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	if _, err := s.Pool.Exec("UPDATE songs SET path = ? WHERE id = 'so-1'", link); err != nil {
		t.Fatal(err)
	}
	s.evictSong("so-1")
	rec := getURL(t, s, mintStreamURL(t, s, tok, "so-1"))
	if strings.Contains(rec.Body.String(), "root:x") || !strings.Contains(rec.Body.String(), "could not read file") {
		t.Fatalf("stream through an escaping link: status %d body %q", rec.Code, rec.Body.String())
	}
}
