package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestUnderJournal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stream"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}

	t.Setenv("JOURNAL_STREAM", "")
	if underJournal(f) {
		t.Error("journal detected without JOURNAL_STREAM")
	}
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)))
	if !underJournal(f) {
		t.Error("journal not detected for the matching stream")
	}
	// A stream inherited from systemd but redirected elsewhere keeps the
	// variable; the inode no longer matches.
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)+1))
	if underJournal(f) {
		t.Error("journal detected for a different stream")
	}
}
