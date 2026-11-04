package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The change feed (getLibraryChanges) needs to know when each artist, album,
// song, playlist and starred item last changed, and which ones were removed.
// Every such row carries updated_ms, and deletions are kept in
// change_tombstones. Triggers maintain both, so the scanner, the watcher and
// the handlers need no change-tracking code of their own.
//
// The triggers are dropped and created again on every start, so editing them
// here takes effect without a migration.

// changeFloorKey stores the oldest since value the feed can answer. A client
// that last synced before it is told to resync fully, because tombstones
// older than the floor have been pruned.
const changeFloorKey = "change_floor_ms"

// changeRetention is how long tombstones are kept.
const changeRetention = 90 * 24 * time.Hour

// nowMsSQL is the current time in epoch milliseconds, read inside SQLite.
const nowMsSQL = "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"

type changeTable struct {
	name string
	// kind is the name written to change_tombstones.
	kind string
	// cols are the values whose change bumps a row. Timestamps that the
	// scanner refreshes on every pass (created_at, bpm_at) are left out.
	cols []string
	// key is the WHERE condition that finds a row, with ref as the trigger
	// row (NEW or OLD). Starred rows are keyed by user and item.
	key func(ref string) string
	// owner is the SQL for the tombstone's user_id: NULL means every account
	// may see the deletion, otherwise only that account.
	owner string
	// parents bumps the rows that contain this one, for example the album and
	// artist of a song. It may be nil.
	parents func(ref string) []string
}

func byID(ref string) string { return "id = " + ref + ".id" }

var changeTables = []changeTable{
	{
		name:  "artists",
		kind:  "artist",
		cols:  []string{"name", "mbid"},
		key:   byID,
		owner: "NULL",
	},
	{
		name:  "albums",
		kind:  "album",
		cols:  []string{"title", "artist", "artist_id", "year", "cover_art", "mbid"},
		key:   byID,
		owner: "NULL",
	},
	{
		name: "songs",
		kind: "song",
		cols: []string{
			"path", "title", "artist", "artist_id", "album", "album_id", "genre",
			"track_number", "disc_number", "year", "duration_ms", "bitrate", "filesize",
			"suffix", "content_type", "cover_art", "mtime", "mbid", "tags", "meter",
			"bpm", "bpm_source", "rg_track_gain", "rg_album_gain", "rg_track_peak",
			"rg_album_peak", "rg_base_gain",
		},
		key:   byID,
		owner: "NULL",
		parents: func(ref string) []string {
			return []string{
				"UPDATE albums SET updated_ms = " + nowMsSQL + " WHERE id = " + ref + ".album_id",
				"UPDATE artists SET updated_ms = " + nowMsSQL + " WHERE id = " + ref + ".artist_id",
			}
		},
	},
	{
		name:  "playlists",
		kind:  "playlist",
		cols:  []string{"name", "comment", "public"},
		key:   byID,
		owner: "OLD.owner_id",
	},
	{
		// Starred rows are per account. Restarring rewrites starred_at, which
		// counts as a change for that account.
		name:  "starred",
		kind:  "starred",
		cols:  []string{"starred_at"},
		key:   func(ref string) string { return "user_id IS " + ref + ".user_id AND id = " + ref + ".id" },
		owner: "OLD.user_id",
	},
}

// changedWhen is true when any tracked column differs between OLD and NEW.
func changedWhen(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "OLD." + c + " IS NOT NEW." + c
	}
	return strings.Join(parts, " OR ")
}

// changeTriggerSQL returns the drop and create statements for every change
// trigger, in the order they must run.
func changeTriggerSQL() []string {
	var out []string
	for _, t := range changeTables {
		stamp := func(ref string) string {
			return "UPDATE " + t.name + " SET updated_ms = " + nowMsSQL + " WHERE " + t.key(ref)
		}
		parents := func(ref string) []string {
			if t.parents == nil {
				return nil
			}
			return t.parents(ref)
		}
		ins := "chg_" + t.name + "_ins"
		upd := "chg_" + t.name + "_upd"
		del := "chg_" + t.name + "_del"

		body := append([]string{stamp("NEW")}, parents("NEW")...)
		out = append(out,
			"DROP TRIGGER IF EXISTS "+ins,
			"CREATE TRIGGER "+ins+" AFTER INSERT ON "+t.name+" BEGIN\n"+joinStmts(body)+"\nEND",
		)

		body = append([]string{stamp("NEW")}, parents("OLD")...)
		body = append(body, parents("NEW")...)
		out = append(out,
			"DROP TRIGGER IF EXISTS "+upd,
			"CREATE TRIGGER "+upd+" AFTER UPDATE ON "+t.name+" WHEN "+changedWhen(t.cols)+" BEGIN\n"+joinStmts(body)+"\nEND",
		)

		body = append([]string{
			"INSERT INTO change_tombstones (kind, item_id, user_id, deleted_ms) VALUES ('" +
				t.kind + "', OLD.id, " + t.owner + ", " + nowMsSQL + ")",
		}, parents("OLD")...)
		out = append(out,
			"DROP TRIGGER IF EXISTS "+del,
			"CREATE TRIGGER "+del+" AFTER DELETE ON "+t.name+" BEGIN\n"+joinStmts(body)+"\nEND",
		)
	}

	// A playlist's updated_ms moves when its entries are added or removed.
	// Reordering is a delete and an insert, so it is covered too.
	for _, op := range []struct{ suffix, ref string }{{"ins", "NEW"}, {"del", "OLD"}} {
		name := "chg_playlist_songs_" + op.suffix
		event := "INSERT"
		if op.suffix == "del" {
			event = "DELETE"
		}
		out = append(out,
			"DROP TRIGGER IF EXISTS "+name,
			"CREATE TRIGGER "+name+" AFTER "+event+" ON playlist_songs BEGIN\n"+
				"UPDATE playlists SET updated_ms = "+nowMsSQL+" WHERE id = "+op.ref+".playlist_id;\nEND",
		)
	}
	return out
}

func joinStmts(stmts []string) string {
	var b strings.Builder
	for _, s := range stmts {
		b.WriteString(s)
		b.WriteString(";\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// migrateChangeFeed adds the change-feed columns and tables to a database
// created before them, backfills updated_ms, installs the triggers and prunes
// old tombstones. It is idempotent.
func migrateChangeFeed(d *sql.DB) error {
	for _, t := range changeTables {
		found, _, err := tableColumn(d, t.name, "updated_ms")
		if err != nil {
			return err
		}
		if found {
			continue
		}
		// The backfill stamps existing rows as changed now, so the first
		// sync after the upgrade sees the whole library.
		for _, q := range []string{
			"ALTER TABLE " + t.name + " ADD COLUMN updated_ms INTEGER NOT NULL DEFAULT 0",
			"UPDATE " + t.name + " SET updated_ms = " + nowMsSQL,
		} {
			if _, err := d.Exec(q); err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
		}
	}

	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS change_tombstones (
		seq        INTEGER PRIMARY KEY AUTOINCREMENT,
		kind       TEXT NOT NULL,
		item_id    TEXT NOT NULL,
		user_id    INTEGER,
		deleted_ms INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := d.Exec("CREATE INDEX IF NOT EXISTS idx_change_tombstones_ms ON change_tombstones(deleted_ms)"); err != nil {
		return err
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range changeTriggerSQL() {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("change triggers: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return PruneChangeTombstones(context.Background(), d, time.Now())
}

// ChangeFloor returns the epoch-millisecond floor below which the change feed
// cannot answer incrementally. It is 0 when nothing has been pruned or reset.
func ChangeFloor(ctx context.Context, d *sql.DB) (int64, error) {
	var v string
	err := d.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", changeFloorKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// PruneChangeTombstones deletes tombstones older than the retention period
// and raises the floor to match. The floor never moves backwards.
func PruneChangeTombstones(ctx context.Context, d *sql.DB, now time.Time) error {
	floor, err := ChangeFloor(ctx, d)
	if err != nil {
		return err
	}
	cutoff := now.Add(-changeRetention).UnixMilli()
	if cutoff > floor {
		floor = cutoff
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM change_tombstones WHERE deleted_ms < ?", floor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		changeFloorKey, strconv.FormatInt(floor, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// raiseChangeFloor moves the floor to at least ms, inside tx. A library reset
// uses it, so every client resyncs.
func raiseChangeFloor(tx *sql.Tx, ms int64) error {
	var v string
	err := tx.QueryRow("SELECT value FROM settings WHERE key = ?", changeFloorKey).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if cur, _ := strconv.ParseInt(v, 10, 64); cur >= ms {
		return nil
	}
	_, err = tx.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		changeFloorKey, strconv.FormatInt(ms, 10))
	return err
}
