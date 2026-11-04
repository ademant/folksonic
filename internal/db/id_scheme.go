package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// idScheme names how library IDs are derived. The stored value is compared
// with this constant, so a database from an earlier scheme is reset once.
const (
	idSchemeKey = "id_scheme"
	idScheme    = "sha256-32"
)

// migrateIDScheme clears the library when the stored ID scheme is not the
// current one. IDs change with the scheme, and a rescan would otherwise leave
// the old rows beside the new ones, since songs are pruned by path. The
// library is rebuilt by the next scan. Playlists are kept, but their songs and
// starred items refer to IDs that no longer exist, so those rows are removed.
// It runs once per database.
func migrateIDScheme(d *sql.DB) error {
	var stored string
	err := d.QueryRow("SELECT value FROM settings WHERE key = ?", idSchemeKey).Scan(&stored)
	if err == nil && stored == idScheme {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM playlist_songs",
		"DELETE FROM starred",
		"DELETE FROM songs",
		"DELETE FROM albums",
		"DELETE FROM artists",
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		idSchemeKey, idScheme); err != nil {
		return err
	}
	// Every client's sync point predates the reset, so the change feed must
	// tell them to rebuild rather than apply changes to IDs that are gone.
	if err := raiseChangeFloor(tx, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	logging.Infof("library IDs use scheme %s: the library is cleared and will be rebuilt by the next scan", idScheme)
	return nil
}
