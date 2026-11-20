package db

import (
	"database/sql"
	"fmt"
)

// schemaBaseline is the user_version stamped once the probe-based steps in
// migrateLegacy have run. Databases below it run those steps, databases at or
// above it skip them.
const schemaBaseline = 1

// migration is one numbered schema step. It runs in one transaction with its
// user_version bump, so a failed step leaves the previous version and no partial
// change (#115). A step that cannot run inside a transaction must be idempotent
// and say so here.
type migration struct {
	name string
	up   func(tx *sql.Tx) error
}

// migrations are the numbered steps after the baseline, applied in order. Add
// new ones at the end; never change or reorder one that has shipped.
var migrations = []migration{
	{"song tempo in the change feed", migrateTempoChanges},
	{"play counts per user and song", migratePlayCounts},
	{"bookmarks per user and song", migrateBookmarks},
	{"play queue per user", migratePlayQueue},
	{"lyrics from tags", migrateLyrics},
	{"device authorization grant and tokens", migrateDeviceGrants},
}

// migrateBookmarks (version 4, #139) adds one bookmark per user and song. The
// table is created only when missing, so the step is idempotent.
func migrateBookmarks(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS bookmarks (
		user_id      INTEGER REFERENCES users(id) ON DELETE CASCADE,
		song_id      TEXT NOT NULL,
		position_ms  INTEGER NOT NULL,
		comment      TEXT NOT NULL DEFAULT '',
		created_ms   INTEGER NOT NULL,
		changed_ms   INTEGER NOT NULL,
		PRIMARY KEY (user_id, song_id)
	)`)
	return err
}

// migratePlayQueue (version 5, #138) adds the saved play queue per user: the
// state row and its songs in order. Both are created only when missing.
func migratePlayQueue(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS play_queues (
			user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			current_song   TEXT NOT NULL DEFAULT '',
			current_index  INTEGER NOT NULL DEFAULT 0,
			position_ms    INTEGER NOT NULL DEFAULT 0,
			changed_ms     INTEGER NOT NULL,
			changed_by     TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS play_queue_items (
			user_id  INTEGER REFERENCES users(id) ON DELETE CASCADE,
			pos      INTEGER NOT NULL,
			song_id  TEXT NOT NULL,
			PRIMARY KEY (user_id, pos)
		)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// migratePlayCounts (version 3, #136) adds the per-user play counts written by
// scrobble and the short-lived keys that make a retried submission harmless.
// Both tables reference users and are created only when missing, so the step
// is idempotent.
func migratePlayCounts(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS play_counts (
			user_id         INTEGER REFERENCES users(id) ON DELETE CASCADE,
			song_id         TEXT NOT NULL,
			play_count      INTEGER NOT NULL,
			last_played_ms  INTEGER NOT NULL,
			PRIMARY KEY (user_id, song_id)
		)`,
		`CREATE TABLE IF NOT EXISTS play_keys (
			user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
			song_id     TEXT NOT NULL,
			played_ms   INTEGER NOT NULL,
			seen_ms     INTEGER NOT NULL,
			PRIMARY KEY (user_id, song_id, played_ms)
		)`,
		`CREATE INDEX IF NOT EXISTS play_keys_seen ON play_keys(seen_ms)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// migrateTempoChanges (version 2, #127) marks the songs that gain
// tinysonicTempo as changed, so clients that sync incrementally fetch them
// again. Albums and artists are unchanged.
func migrateTempoChanges(tx *sql.Tx) error {
	_, err := tx.Exec("UPDATE songs SET updated_ms = " + nowMsSQL + `
		WHERE bpm_confidence IS NOT NULL
		   OR id IN (SELECT song_id FROM song_values WHERE name IN ` + sqlNameList(SongObjectValues) + ")")
	return err
}

// userVersion reads PRAGMA user_version, which is 0 on a database that has not
// been stamped.
func userVersion(d *sql.DB) (int, error) {
	var v int
	err := d.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// applyMigrations stamps the baseline when it is missing, then runs each step
// whose version is above the current one.
func applyMigrations(d *sql.DB, steps []migration) error {
	cur, err := userVersion(d)
	if err != nil {
		return err
	}
	if cur < schemaBaseline {
		if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaBaseline)); err != nil {
			return fmt.Errorf("stamping schema baseline: %w", err)
		}
		cur = schemaBaseline
	}
	for i, m := range steps {
		want := schemaBaseline + i + 1
		if cur >= want {
			continue
		}
		if err := runMigration(d, m, want); err != nil {
			return fmt.Errorf("migration %d (%s): %w", want, m.name, err)
		}
		cur = want
	}
	return nil
}

// runMigration applies one step and its version bump in a single transaction.
func runMigration(d *sql.DB, m migration, version int) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.up(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateLyrics (version 6, #142) adds the lyrics of each song. Every mtime is
// reset to 0, so the next scan reads the tags again and fills the table; the
// audio itself is not reanalysed. The rows go with their song.
func migrateLyrics(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS song_lyrics (
		song_id  TEXT PRIMARY KEY REFERENCES songs(id) ON DELETE CASCADE,
		text     TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE songs SET mtime = 0")
	return err
}

// migrateDeviceGrants (version 7, #93) adds the device authorization grant: the
// pending device codes, the grants an approved code creates, the refresh tokens
// already rotated away (kept to recognise a reuse) and the access tokens. Each
// table is created only when missing.
func migrateDeviceGrants(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS oauth_device_codes (
			device_hash   TEXT PRIMARY KEY,
			user_code     TEXT NOT NULL UNIQUE,
			label         TEXT NOT NULL DEFAULT '',
			user_id       INTEGER REFERENCES users(id) ON DELETE CASCADE,
			expires_ms    INTEGER NOT NULL,
			interval_ms   INTEGER NOT NULL,
			last_poll_ms  INTEGER NOT NULL DEFAULT 0,
			created_ms    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_grants (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			label         TEXT NOT NULL DEFAULT '',
			jkt           TEXT NOT NULL,
			refresh_hash  TEXT NOT NULL UNIQUE,
			created_ms    INTEGER NOT NULL,
			last_used_ms  INTEGER NOT NULL,
			revoked_ms    INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_refresh_used (
			refresh_hash  TEXT PRIMARY KEY,
			grant_id      INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
			used_ms       INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_refresh_used_age ON oauth_refresh_used(used_ms)`,
		`CREATE TABLE IF NOT EXISTS oauth_access_tokens (
			token_hash    TEXT PRIMARY KEY,
			grant_id      INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
			expires_ms    INTEGER NOT NULL
		)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}
