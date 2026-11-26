package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Remote playback control (#157). A player (an app with its own player, such as
// DanceSonic Studio) reports its state to folksonic and long-polls for commands.
// A controller (any app of the same user) sends commands to a player by its ID.
// Folksonic coordinates the state and the commands; the audio still comes from
// the library. Queue changes go through the saved play queue, not here.

const (
	// remoteCommandTTL is how long an unapplied command stays deliverable.
	// Older ones are never delivered, so a reconnecting player does not replay
	// stale commands.
	remoteCommandTTL = 5 * time.Minute
	// remoteOnlineWindow is how recently a player must have reported to count as online.
	remoteOnlineWindow = 60 * time.Second
	// maxRemoteWait bounds one long poll.
	maxRemoteWait = 30 * time.Second
	// maxPendingRemote bounds the commands waiting for one player.
	maxPendingRemote = 100
	// maxPlayerName bounds the name a player reports for itself, in characters.
	maxPlayerName = 64
	// maxRemotePosition bounds a position, in milliseconds (about 1000 hours).
	maxRemotePosition = int64(1000 * 3600 * 1000)
	// maxRemotePlayers bounds the players one account has on record (#169). A
	// new player past it replaces the one that reported longest ago, if that
	// one is offline; otherwise it is refused.
	maxRemotePlayers = 16
	// maxRemoteField bounds song_id and controller, in bytes (#169).
	maxRemoteField = 64
)

// remoteCommandNames are the commands a controller may send.
var remoteCommandNames = map[string]bool{
	"play": true, "pause": true, "seek": true, "next": true, "previous": true,
}

// remoteStatuses are the states a player may report.
var remoteStatuses = map[string]bool{"playing": true, "paused": true, "stopped": true}

// remoteCommand is one command as a player receives it.
type remoteCommand struct {
	Seq        int64
	Command    string
	PositionMs int64
	SongID     string
	Controller string
	CreatedMs  int64
}

// validPlayerID accepts 1–64 characters of letters, digits, dot, dash and underscore.
func validPlayerID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// APIRemotePlayers lists the caller's players with their last reported state.
func (s *State) APIRemotePlayers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.Pool.QueryContext(r.Context(),
		"SELECT player_id, name, status, song_id, position_ms, reported_ms, applied_seq, controller "+
			"FROM remote_players WHERE user_id = ? ORDER BY name, player_id", u.ID)
	if err != nil {
		logging.Errorf("remote players: %v", err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	defer rows.Close()
	now := time.Now().UnixMilli()
	out := []J{}
	for rows.Next() {
		var id, name, status, song, controller string
		var position, reported, applied int64
		if err := rows.Scan(&id, &name, &status, &song, &position, &reported, &applied, &controller); err != nil {
			writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
			return
		}
		out = append(out, J{
			"id":         id,
			"name":       name,
			"status":     status,
			"songId":     song,
			"positionMs": position,
			"reportedAt": isoTime(reported),
			"appliedSeq": applied,
			"controller": controller,
			"online":     now-reported <= remoteOnlineWindow.Milliseconds(),
		})
	}
	if err := rows.Err(); err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"players": out})
}

// APIRemoteReport is a player's state report. It creates or updates the player.
// applied_seq only moves forward, so an old report cannot make commands replay.
func (s *State) APIRemoteReport(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if !validPlayerID(pid) {
		writeAPIError(w, http.StatusBadRequest, "player id must be 1 to 64 letters, digits, dots, dashes or underscores")
		return
	}
	var in struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		SongID     string `json:"song_id"`
		PositionMs int64  `json:"position_ms"`
		AppliedSeq int64  `json:"applied_seq"`
		Controller string `json:"controller"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if !remoteStatuses[in.Status] {
		writeAPIError(w, http.StatusBadRequest, "status must be playing, paused or stopped")
		return
	}
	if in.PositionMs < 0 || in.PositionMs > maxRemotePosition || in.AppliedSeq < 0 {
		writeAPIError(w, http.StatusBadRequest, "position_ms or applied_seq is out of range")
		return
	}
	if len(in.SongID) > maxRemoteField || len(in.Controller) > maxRemoteField {
		writeAPIError(w, http.StatusBadRequest, "song_id and controller are at most 64 bytes")
		return
	}
	name := string([]rune(in.Name)[:min(len([]rune(in.Name)), maxPlayerName)])
	switch err := s.makeRoomForPlayer(r.Context(), u.ID, pid, time.Now()); {
	case errors.Is(err, errTooManyPlayers):
		writeAPIErrorCode(w, http.StatusConflict, codeTooManyPlayers, "this account has 16 players that reported within the last minute; no room for another")
		return
	case err != nil:
		logging.Errorf("remote report %s: %v", pid, err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	_, err := s.Pool.ExecContext(r.Context(),
		"INSERT INTO remote_players (user_id, player_id, name, status, song_id, position_ms, reported_ms, applied_seq, controller) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT(user_id, player_id) DO UPDATE SET "+
			"name = excluded.name, status = excluded.status, song_id = excluded.song_id, "+
			"position_ms = excluded.position_ms, reported_ms = excluded.reported_ms, "+
			"applied_seq = MAX(remote_players.applied_seq, excluded.applied_seq), controller = excluded.controller",
		u.ID, pid, name, in.Status, in.SongID, in.PositionMs, time.Now().UnixMilli(), in.AppliedSeq, in.Controller)
	if err != nil {
		logging.Errorf("remote report %s: %v", pid, err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"ok": true})
}

// APIRemoteCommands is the player's long poll. It returns the commands after
// the given sequence number, or after the player's applied sequence if that is
// higher, waiting up to wait seconds when there are none yet.
func (s *State) APIRemoteCommands(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if !validPlayerID(pid) {
		writeAPIError(w, http.StatusBadRequest, "player id is not valid")
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") != "" && (err != nil || after < 0) {
		writeAPIError(w, http.StatusBadRequest, "after must be a sequence number")
		return
	}
	wait := time.Duration(0)
	if raw := r.URL.Query().Get("wait"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs < 0 {
			writeAPIError(w, http.StatusBadRequest, "wait must be a number of seconds")
			return
		}
		wait = min(time.Duration(secs)*time.Second, maxRemoteWait)
	}
	ctx := r.Context()
	applied, err := remoteAppliedSeq(ctx, s.Pool, u.ID, pid)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	if applied > after {
		after = applied
	}
	deadline := time.Now().Add(wait)
	for {
		cmds, err := loadRemoteCommands(ctx, s.Pool, u.ID, pid, after, time.Now().Add(-remoteCommandTTL).UnixMilli())
		if err != nil {
			logging.Errorf("remote commands %s: %v", pid, err)
			writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
			return
		}
		if len(cmds) > 0 || !time.Now().Before(deadline) {
			out := make([]J, 0, len(cmds))
			for _, c := range cmds {
				out = append(out, J{
					"seq":        c.Seq,
					"command":    c.Command,
					"positionMs": c.PositionMs,
					"songId":     c.SongID,
					"controller": c.Controller,
					"sentAt":     isoTime(c.CreatedMs),
				})
			}
			writeJSON(w, http.StatusOK, J{"commands": out})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// APIRemoteSend queues a command for a player. Any of the user's apps may send.
// The last command wins: the player obeys commands in sequence, and reports
// which controller it obeyed last.
func (s *State) APIRemoteSend(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	pid := r.PathValue("id")
	if !validPlayerID(pid) {
		writeAPIError(w, http.StatusBadRequest, "player id is not valid")
		return
	}
	var in struct {
		Command    string `json:"command"`
		PositionMs int64  `json:"position_ms"`
		SongID     string `json:"song_id"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if !remoteCommandNames[in.Command] {
		writeAPIError(w, http.StatusBadRequest, "command must be play, pause, seek, next or previous")
		return
	}
	if in.Command == "seek" && (in.PositionMs < 0 || in.PositionMs > maxRemotePosition) {
		writeAPIError(w, http.StatusBadRequest, "seek needs position_ms between 0 and 1000 hours")
		return
	}
	if len(in.SongID) > maxRemoteField {
		writeAPIError(w, http.StatusBadRequest, "song_id is at most 64 bytes")
		return
	}
	seq, err := repoSendRemoteCommand(r.Context(), s.Pool, u, pid, in.Command, in.PositionMs, in.SongID, time.Now())
	if errors.Is(err, errUnknownPlayer) {
		writeAPIError(w, http.StatusNotFound, "no such player; it has to report its state first")
		return
	}
	if errors.Is(err, errTooManyCommands) {
		writeAPIError(w, http.StatusTooManyRequests, "too many commands are waiting for this player")
		return
	}
	if err != nil {
		logging.Errorf("remote send %s: %v", pid, err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"seq": seq})
}

var (
	errUnknownPlayer   = errors.New("unknown player")
	errTooManyCommands = errors.New("too many pending commands")
	errTooManyPlayers  = errors.New("too many players")
)

// makeRoomForPlayer lets pid report for userID. A known player always may. A
// new one past maxRemotePlayers replaces the player that reported longest
// ago, with its commands, when that one is offline (#169).
func (s *State) makeRoomForPlayer(ctx context.Context, userID int64, pid string, now time.Time) error {
	tx, err := s.Pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var known, count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FILTER (WHERE player_id = ?), COUNT(*) FROM remote_players WHERE user_id = ?",
		pid, userID).Scan(&known, &count); err != nil {
		return err
	}
	if known > 0 || count < maxRemotePlayers {
		return nil
	}
	var oldest string
	var reported int64
	if err := tx.QueryRowContext(ctx,
		"SELECT player_id, reported_ms FROM remote_players WHERE user_id = ? ORDER BY reported_ms, player_id LIMIT 1",
		userID).Scan(&oldest, &reported); err != nil {
		return err
	}
	if reported >= now.Add(-remoteOnlineWindow).UnixMilli() {
		return errTooManyPlayers
	}
	for _, q := range []string{
		"DELETE FROM remote_commands WHERE user_id = ? AND player_id = ?",
		"DELETE FROM remote_players WHERE user_id = ? AND player_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, userID, oldest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// repoSendRemoteCommand stores one command, after dropping expired commands for
// that player. It refuses unknown players and a full queue.
func repoSendRemoteCommand(ctx context.Context, db *sql.DB, u users.User, pid, command string, position int64, songID string, now time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var known int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM remote_players WHERE user_id = ? AND player_id = ?", u.ID, pid).Scan(&known); err != nil {
		return 0, err
	}
	if known == 0 {
		return 0, errUnknownPlayer
	}
	// Expired commands of every player of the account go, not only this
	// one's, so a player that stopped polling does not keep its queue (#169).
	cutoff := now.Add(-remoteCommandTTL).UnixMilli()
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM remote_commands WHERE user_id = ? AND created_ms < ?", u.ID, cutoff); err != nil {
		return 0, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM remote_commands WHERE user_id = ? AND player_id = ?", u.ID, pid).Scan(&pending); err != nil {
		return 0, err
	}
	if pending >= maxPendingRemote {
		return 0, errTooManyCommands
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO remote_commands (user_id, player_id, command, position_ms, song_id, controller, created_ms) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		u.ID, pid, command, position, songID, u.Username, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// remoteAppliedSeq is the last command sequence the player acknowledged.
func remoteAppliedSeq(ctx context.Context, db *sql.DB, userID int64, pid string) (int64, error) {
	var applied int64
	err := db.QueryRowContext(ctx,
		"SELECT applied_seq FROM remote_players WHERE user_id = ? AND player_id = ?", userID, pid).Scan(&applied)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return applied, err
}

// loadRemoteCommands returns the player's commands after seq that are not older
// than cutoffMs, in order.
func loadRemoteCommands(ctx context.Context, db *sql.DB, userID int64, pid string, after, cutoffMs int64) ([]remoteCommand, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT seq, command, position_ms, song_id, controller, created_ms FROM remote_commands "+
			"WHERE user_id = ? AND player_id = ? AND seq > ? AND created_ms >= ? ORDER BY seq LIMIT 100",
		userID, pid, after, cutoffMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remoteCommand
	for rows.Next() {
		var c remoteCommand
		if err := rows.Scan(&c.Seq, &c.Command, &c.PositionMs, &c.SongID, &c.Controller, &c.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
