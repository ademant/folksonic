package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// migrateSongTagsColumn adds songs.tags, which holds the allowlisted file tags
// of each song as a JSON object. It is idempotent.
func migrateSongTagsColumn(d *sql.DB) error {
	found, _, err := tableColumn(d, "songs", "tags")
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = d.Exec("ALTER TABLE songs ADD COLUMN tags TEXT")
	return err
}

const songTagsSetting = "song_tags"

// SyncSongTags records the allowlist of file tags and returns whether it changed.
// When it changed, every mtime is reset to 0, so the next scan re-reads the tags
// of every file. An absent setting counts as an empty list, so a library that
// never used song_tags is not rescanned. tags must already be normalised.
func SyncSongTags(ctx context.Context, d *sql.DB, tags []string) (bool, error) {
	want, err := json.Marshal(tags)
	if err != nil {
		return false, err
	}
	var stored string
	err = d.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", songTagsSetting).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		stored = "[]"
	} else if err != nil {
		return false, err
	}
	var had []string
	if err := json.Unmarshal([]byte(stored), &had); err != nil {
		return false, fmt.Errorf("stored song_tags: %w", err)
	}
	if slices.Equal(had, tags) {
		return false, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE songs SET mtime = 0"); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		songTagsSetting, string(want)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
