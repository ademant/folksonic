package db

import (
	"database/sql"
	"strings"
)

// songsFTSColumns are the songs columns that feed songs_fts. The update
// trigger fires only when one of them changes, so bookkeeping updates (mtime,
// created_at, tags) do not rewrite the search index.
const songsFTSColumns = "id, title, artist, album, genre"

const songsAUTrigger = `CREATE TRIGGER songs_au AFTER UPDATE OF ` + songsFTSColumns + ` ON songs BEGIN
    DELETE FROM songs_fts WHERE id = OLD.id;
    INSERT INTO songs_fts (id, title, artist, album, genre)
    VALUES (NEW.id, NEW.title, NEW.artist, NEW.album, COALESCE(NEW.genre, ''));
END`

// migrateSongCreated narrows the songs FTS update trigger to the indexed
// columns and adds songs.created_at, initialised from each file's mtime. Both
// steps run in one transaction and are idempotent.
func migrateSongCreated(d *sql.DB) error {
	var trig string
	if err := d.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'songs_au'").Scan(&trig); err != nil {
		return err
	}
	narrowTrigger := !strings.Contains(trig, "UPDATE OF")
	found, _, err := tableColumn(d, "songs", "created_at")
	if err != nil {
		return err
	}
	if !narrowTrigger && found {
		return nil
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Replace the trigger first, so the backfill below does not fire it.
	if narrowTrigger {
		if _, err := tx.Exec("DROP TRIGGER songs_au"); err != nil {
			return err
		}
		if _, err := tx.Exec(songsAUTrigger); err != nil {
			return err
		}
	}
	if !found {
		if _, err := tx.Exec("ALTER TABLE songs ADD COLUMN created_at TEXT"); err != nil {
			return err
		}
	}
	// mtime 0 means "re-read on next scan" (set by earlier migrations); those
	// rows get the migration time instead of 1970.
	if _, err := tx.Exec(`UPDATE songs SET created_at = CASE
		WHEN mtime > 0 THEN strftime('%Y-%m-%dT%H:%M:%SZ', mtime, 'unixepoch')
		ELSE strftime('%Y-%m-%dT%H:%M:%SZ', 'now') END
		WHERE created_at IS NULL`); err != nil {
		return err
	}
	return tx.Commit()
}
