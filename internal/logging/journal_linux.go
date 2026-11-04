package logging

import (
	"fmt"
	"os"
	"syscall"
)

// underJournal reports whether f is the stream systemd connected to the
// journal. systemd sets JOURNAL_STREAM to the device and inode of that stream
// ("dev:ino"); comparing them with f's tells a service's own stderr apart
// from one redirected elsewhere by a child or the shell.
func underJournal(f *os.File) bool {
	want := os.Getenv("JOURNAL_STREAM")
	if want == "" {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return false
	}
	return want == fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}
