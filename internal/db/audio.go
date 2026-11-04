package db

import (
	"database/sql"
	"fmt"
)

// audioColumns are the songs columns for ReplayGain, meter and BPM.
var audioColumns = []struct{ name, typ string }{
	{"rg_track_gain", "REAL"}, {"rg_album_gain", "REAL"}, {"rg_track_peak", "REAL"},
	{"rg_album_peak", "REAL"}, {"rg_base_gain", "REAL"}, {"meter", "TEXT"},
	{"bpm", "REAL"}, {"bpm_source", "TEXT"}, {"bpm_confidence", "REAL"}, {"bpm_at", "TEXT"},
}

// migrateAudioColumns adds the loudness, meter and BPM columns to an existing
// songs table and marks every song for one tag re-read (mtime = 0), all in
// one transaction, so an interrupted start cannot leave the columns added but
// the songs unmarked. The songs FTS trigger ignores mtime (migrateSongCreated
// runs first), so the reset does not rewrite the search index.
func migrateAudioColumns(d *sql.DB) error {
	var missing []string
	for _, c := range audioColumns {
		found, _, err := tableColumn(d, "songs", c.name)
		if err != nil {
			return err
		}
		if !found {
			missing = append(missing, fmt.Sprintf("ALTER TABLE songs ADD COLUMN %s %s", c.name, c.typ))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range missing {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE songs SET mtime = 0"); err != nil {
		return err
	}
	return tx.Commit()
}
