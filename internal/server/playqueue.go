package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// maxQueueSongs bounds one saved queue (#138).
const maxQueueSongs = 10000

// playQueue is the saved queue of one user: the song ids in order, the current
// one by id and by index, the position in it, and who saved it last.
type playQueue struct {
	current      string
	currentIndex int
	positionMs   int64
	changedMs    int64
	changedBy    string
	songs        []string
}

// repoSavePlayQueue replaces the user's queue. An empty queue clears it.
func repoSavePlayQueue(ctx context.Context, db *sql.DB, userID int64, q playQueue) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		"DELETE FROM play_queue_items WHERE user_id = ?",
		"DELETE FROM play_queues WHERE user_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, stmt, userID); err != nil {
			return err
		}
	}
	if len(q.songs) == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO play_queues (user_id, current_song, current_index, position_ms, changed_ms, changed_by) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		userID, q.current, q.currentIndex, q.positionMs, q.changedMs, q.changedBy); err != nil {
		return err
	}
	for i, id := range q.songs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO play_queue_items (user_id, pos, song_id) VALUES (?, ?, ?)", userID, i, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// repoLoadPlayQueue reads the user's queue, or nil when none is saved.
func repoLoadPlayQueue(ctx context.Context, db *sql.DB, userID int64) (*playQueue, error) {
	var q playQueue
	err := db.QueryRowContext(ctx,
		"SELECT current_song, current_index, position_ms, changed_ms, changed_by FROM play_queues WHERE user_id = ?",
		userID).Scan(&q.current, &q.currentIndex, &q.positionMs, &q.changedMs, &q.changedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		"SELECT song_id FROM play_queue_items WHERE user_id = ? ORDER BY pos", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		q.songs = append(q.songs, id)
	}
	return &q, rows.Err()
}

// queueSongs returns the queue's songs that are still in the library, in
// queue order, and the index of the current song among them. When the current
// song has left the library, the index is that of the next song kept.
func (s *State) queueSongs(ctx context.Context, userID int64, q *playQueue) ([]models.Song, int, error) {
	if len(q.songs) == 0 {
		return nil, 0, nil
	}
	args := make([]any, 0, len(q.songs))
	for _, id := range q.songs {
		args = append(args, id)
	}
	found, err := queryManySongs(ctx, s.Pool,
		"SELECT "+songCols+" FROM songs s WHERE s.id IN ("+placeholders(len(q.songs))+")", args...)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]models.Song, len(found))
	for _, sg := range found {
		byID[sg.ID] = sg
	}
	kept := make([]models.Song, 0, len(q.songs))
	index := 0
	for i, id := range q.songs {
		if i == q.currentIndex {
			index = len(kept)
		}
		if sg, ok := byID[id]; ok {
			kept = append(kept, sg)
		}
	}
	return kept, index, nil
}

// queueEntries converts the kept songs of the queue, with the caller's plays.
func (s *State) queueEntries(ctx context.Context, userID int64, songs []models.Song) ([]J, error) {
	plays, err := s.playsFor(ctx, userID, songs)
	if err != nil {
		return nil, err
	}
	return s.songsJSON(songs, plays), nil
}

// SavePlayQueue replaces the caller's queue with the id parameters in order.
// current must be one of them. No id clears the queue (OpenSubsonic).
func (s *State) SavePlayQueue(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ids := queryAll(r, "id")
	if len(ids) > maxQueueSongs {
		writeError(w, 10, "Invalid parameter: the queue has too many songs")
		return
	}
	position, ok := positionParam(w, r)
	if !ok {
		return
	}
	q := playQueue{positionMs: position, changedMs: time.Now().UnixMilli(), changedBy: queryFirst(r, "c"), songs: ids}
	if len(ids) > 0 {
		q.current = queryFirst(r, "current")
		if q.current == "" {
			writeError(w, 10, "Required parameter is missing: current")
			return
		}
		q.currentIndex = indexOf(ids, q.current)
		if q.currentIndex < 0 {
			writeError(w, 10, "Invalid parameter: current is not in the queue")
			return
		}
	}
	if err := repoSavePlayQueue(r.Context(), s.Pool, caller.ID, q); err != nil {
		s.dbFailed(w, "savePlayQueue", err)
		return
	}
	writeOK(w, J{})
}

// SavePlayQueueByIndex is SavePlayQueue with the current song given by its
// 0-based index in the queue (OpenSubsonic indexBasedQueue).
func (s *State) SavePlayQueueByIndex(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ids := queryAll(r, "id")
	if len(ids) > maxQueueSongs {
		writeError(w, 10, "Invalid parameter: the queue has too many songs")
		return
	}
	position, ok := positionParam(w, r)
	if !ok {
		return
	}
	q := playQueue{positionMs: position, changedMs: time.Now().UnixMilli(), changedBy: queryFirst(r, "c"), songs: ids}
	if len(ids) > 0 {
		raw := queryFirst(r, "currentIndex")
		if raw == "" {
			writeError(w, 10, "Required parameter is missing: currentIndex")
			return
		}
		idx, err := strconv.Atoi(raw)
		if err != nil || idx < 0 || idx >= len(ids) {
			writeError(w, 10, "Invalid parameter: currentIndex is outside the queue")
			return
		}
		q.currentIndex = idx
		q.current = ids[idx]
	}
	if err := repoSavePlayQueue(r.Context(), s.Pool, caller.ID, q); err != nil {
		s.dbFailed(w, "savePlayQueueByIndex", err)
		return
	}
	writeOK(w, J{})
}

// GetPlayQueue returns the caller's saved queue, or an empty playQueue when
// none is saved.
func (s *State) GetPlayQueue(w http.ResponseWriter, r *http.Request) {
	s.playQueueAnswer(w, r, "getPlayQueue", false)
}

// GetPlayQueueByIndex is GetPlayQueue with currentIndex in place of current.
func (s *State) GetPlayQueueByIndex(w http.ResponseWriter, r *http.Request) {
	s.playQueueAnswer(w, r, "getPlayQueueByIndex", true)
}

func (s *State) playQueueAnswer(w http.ResponseWriter, r *http.Request, op string, byIndex bool) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q, err := repoLoadPlayQueue(ctx, s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, op, err)
		return
	}
	key := "playQueue"
	if byIndex {
		key = "playQueueByIndex"
	}
	if q == nil {
		writeOK(w, J{key: J{}})
		return
	}
	songs, index, err := s.queueSongs(ctx, caller.ID, q)
	if err != nil {
		s.dbFailed(w, op, err)
		return
	}
	entries, err := s.queueEntries(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, op, err)
		return
	}
	out := J{
		"position":  q.positionMs,
		"username":  caller.Username,
		"changed":   isoTime(q.changedMs),
		"changedBy": q.changedBy,
		"entry":     entries,
	}
	if byIndex {
		out["currentIndex"] = index
	} else {
		out["current"] = q.current
	}
	writeOK(w, J{key: out})
}

// positionParam reads the optional position in ms. It writes the error and
// returns false when the value is not a number.
func positionParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := queryFirst(r, "position")
	if raw == "" {
		return 0, true
	}
	p, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || p < 0 {
		writeError(w, 10, "Invalid parameter: position")
		return 0, false
	}
	return p, true
}

// indexOf is the first position of v in ids, or -1.
func indexOf(ids []string, v string) int {
	for i, id := range ids {
		if id == v {
			return i
		}
	}
	return -1
}
