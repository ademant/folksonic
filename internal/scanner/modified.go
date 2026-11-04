package scanner

import (
	"database/sql"
	"time"
)

// LibraryModifiedKey is the settings key holding the time of the last change
// to the library (a song inserted, updated or removed by a scan or the
// watcher). Clients read it through tinysonicLibraryInfo to skip syncs.
const LibraryModifiedKey = "library_modified"

// LastScanKey is the settings key holding when the last full scan finished.
const LastScanKey = "last_scan"

// markModified records a library change inside the transaction that made it.
func markModified(tx *sql.Tx) error {
	_, err := tx.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		LibraryModifiedKey, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	return err
}
