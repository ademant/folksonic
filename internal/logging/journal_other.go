//go:build !linux

package logging

import "os"

// underJournal is false outside Linux: there is no systemd journal.
func underJournal(*os.File) bool { return false }
