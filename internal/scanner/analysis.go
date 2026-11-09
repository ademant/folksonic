package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// storeTagValues keeps the analysis tables in step with a re-read file.
//
// When the audio changed, every analysis result and interrupted-attempt count
// of the song is dropped, so the assigned tools run again from scratch. Values read from tags (the key) are then
// stored with tool 'tag', unless an assigned tool already produced the
// feature for this audio: a tool's result replaces the tag's.
func storeTagValues(tx *sql.Tx, songID string, au audioTags, audioChanged bool) error {
	if audioChanged {
		for _, q := range []string{
			"DELETE FROM analysis WHERE song_id = ?",
			"DELETE FROM analysis_attempts WHERE song_id = ?",
			"DELETE FROM song_values WHERE song_id = ?",
		} {
			if _, err := tx.Exec(q, songID); err != nil {
				return err
			}
		}
	}
	var tool sql.NullString
	err := tx.QueryRow("SELECT tool FROM analysis WHERE song_id = ? AND feature = 'key'", songID).Scan(&tool)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if tool.Valid && tool.String != "tag" {
		return nil // a tool's result for this audio wins
	}
	if au.Key == nil {
		if tool.Valid {
			_, err := tx.Exec("DELETE FROM song_values WHERE song_id = ? AND name = 'key'", songID)
			if err == nil {
				_, err = tx.Exec("DELETE FROM analysis WHERE song_id = ? AND feature = 'key'", songID)
			}
			return err
		}
		return nil
	}
	js, err := json.Marshal(au.Key)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO song_values (song_id, name, value, changed_ms) VALUES (?, 'key', ?, ?)
		 ON CONFLICT(song_id, name) DO UPDATE SET value = excluded.value, changed_ms = excluded.changed_ms
		 WHERE song_values.value IS NOT excluded.value`, songID, string(js), now); err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO analysis (song_id, feature, tool, tool_version, analysed_ms) VALUES (?, 'key', 'tag', NULL, ?)
		 ON CONFLICT(song_id, feature) DO UPDATE SET tool = 'tag', tool_version = NULL, analysed_ms = excluded.analysed_ms, error = NULL`,
		songID, now)
	return err
}
