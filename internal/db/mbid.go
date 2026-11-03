package db

import (
	"database/sql"
	"fmt"
)

// migrateMBID adds the MusicBrainz ID columns to artists, albums and songs.
// It is idempotent. When songs gains the column, every mtime is reset to 0: the
// scanner skips files whose mtime is unchanged, so this makes the next scan
// read the tags again and fill in the IDs.
func migrateMBID(d *sql.DB) error {
	added := false
	for _, table := range []string{"artists", "albums", "songs"} {
		found, _, err := tableColumn(d, table, "mbid")
		if err != nil {
			return err
		}
		if found {
			continue
		}
		if _, err := d.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN mbid TEXT", table)); err != nil {
			return err
		}
		if table == "songs" {
			added = true
		}
	}
	if added {
		if _, err := d.Exec("UPDATE songs SET mtime = 0"); err != nil {
			return err
		}
	}
	return nil
}
