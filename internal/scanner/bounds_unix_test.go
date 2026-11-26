//go:build unix

package scanner

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO named cover.jpg is skipped, not opened: opening it would block the
// scan until something writes to it (#198).
func TestFIFOCoverDoesNotBlock(t *testing.T) {
	music := t.TempDir()
	song := filepath.Join(music, "a.mp3")
	if err := os.WriteFile(song, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(music, "cover.jpg"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := findDirCover([]string{music}, song, t.TempDir(), "al-1")
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a FIFO was taken as a cover")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("findDirCover blocked on a FIFO")
	}
}
