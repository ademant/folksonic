package server

import (
	"context"
	"database/sql"
	"time"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// playKeyRetention is how long a (user, song, time) key of a recorded play is
// kept, which is the window in which a client may retry a submission. Keys
// older than this are pruned at startup and on every scrobble. Only the
// counts in play_counts are kept beyond it.
const playKeyRetention = 24 * time.Hour

// A scrobble time more than playFutureSkew ahead is stored as now; one older
// than playMaxAge as now − playMaxAge (#185). Plays queued offline keep their
// time for a year; a future time cannot push a song to the top of the
// recent lists.
const (
	playFutureSkew = 5 * time.Minute
	playMaxAge     = 365 * 24 * time.Hour
)

// clampPlayTime bounds a client's play time, in Unix ms, around nowMs.
func clampPlayTime(t, nowMs int64) int64 {
	switch {
	case t > nowMs+playFutureSkew.Milliseconds():
		return nowMs
	case t < nowMs-playMaxAge.Milliseconds():
		return nowMs - playMaxAge.Milliseconds()
	}
	return t
}

// playStat is one user's count for one song.
type playStat struct {
	Count  int64
	LastMs int64
	// Rating is the caller's star rating (1 to 5, #140), 0 when unrated.
	Rating int64
}

// playStats maps a song ID to the caller's count. A song without plays is
// absent, and a nil map reads as no plays at all.
type playStats map[string]playStat

// playsFor loads the caller's play counts for songs in one query.
func (s *State) playsFor(ctx context.Context, userID int64, songs []models.Song) (playStats, error) {
	ids := make([]any, 0, len(songs))
	for _, sg := range songs {
		ids = append(ids, sg.ID)
	}
	return repoPlayCounts(ctx, s.Pool, userID, ids)
}

// repoPlayCounts reads the counts of ids for one user.
func repoPlayCounts(ctx context.Context, db *sql.DB, userID int64, ids []any) (playStats, error) {
	out := playStats{}
	if len(ids) == 0 {
		return out, nil
	}
	args := append([]any{userID}, ids...)
	rows, err := db.QueryContext(ctx,
		"SELECT song_id, play_count, last_played_ms FROM play_counts "+
			"WHERE user_id = ? AND song_id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p playStat
		if err := rows.Scan(&id, &p.Count, &p.LastMs); err != nil {
			return nil, err
		}
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A song may be rated without any plays, so ratings get their own query.
	rated, err := db.QueryContext(ctx,
		"SELECT song_id, rating FROM user_ratings "+
			"WHERE user_id = ? AND song_id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rated.Close()
	for rated.Next() {
		var id string
		var rating int64
		if err := rated.Scan(&id, &rating); err != nil {
			return nil, err
		}
		p := out[id]
		p.Rating = rating
		out[id] = p
	}
	return out, rated.Err()
}

// repoRecordPlay counts one play of songID for userID at playedMs. keyMs is
// the time the client sent, the retry key: a play whose key is already
// recorded (a retry) is not counted again. The key and the count change in
// one transaction, and the expired keys are pruned in it too.
func repoRecordPlay(ctx context.Context, db *sql.DB, userID int64, songID string, keyMs, playedMs, nowMs int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := pruneKeys(ctx, tx, nowMs); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO play_keys (user_id, song_id, played_ms, seen_ms) VALUES (?, ?, ?, ?) "+
			"ON CONFLICT(user_id, song_id, played_ms) DO NOTHING",
		userID, songID, keyMs, nowMs)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO play_counts (user_id, song_id, play_count, last_played_ms) VALUES (?, ?, 1, ?) "+
				"ON CONFLICT(user_id, song_id) DO UPDATE SET "+
				"play_count = play_count + 1, last_played_ms = MAX(last_played_ms, excluded.last_played_ms)",
			userID, songID, playedMs)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PrunePlayKeys drops the retry keys older than playKeyRetention. It runs at
// startup, so keys do not pile up while no client scrobbles.
func PrunePlayKeys(ctx context.Context, db *sql.DB) error {
	return pruneKeys(ctx, db, time.Now().UnixMilli())
}

// pruneKeys works on a database or a transaction.
func pruneKeys(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, nowMs int64) error {
	_, err := ex.ExecContext(ctx, "DELETE FROM play_keys WHERE seen_ms < ?",
		nowMs-playKeyRetention.Milliseconds())
	return err
}
