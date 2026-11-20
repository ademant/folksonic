package server

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// nowPlayingTTL is how long a player's report counts after its last update. A
// player that closes stops showing as playing after this (#147).
const nowPlayingTTL = 30 * time.Minute

// nowEntry is what one player of one user last reported.
type nowEntry struct {
	songID   string
	playerID string
	at       time.Time
}

// nowPlayingStore holds the reports in memory only; nothing is written to the
// database, and a restart clears it. The zero value is ready to use.
type nowPlayingStore struct {
	mu     sync.Mutex
	byUser map[int64]map[string]nowEntry
}

func (n *nowPlayingStore) set(userID int64, playerID, songID string, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(now)
	if n.byUser == nil {
		n.byUser = map[int64]map[string]nowEntry{}
	}
	m := n.byUser[userID]
	if m == nil {
		m = map[string]nowEntry{}
		n.byUser[userID] = m
	}
	m[playerID] = nowEntry{songID: songID, playerID: playerID, at: now}
}

// list returns the user's live reports, most recent first.
func (n *nowPlayingStore) list(userID int64, now time.Time) []nowEntry {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pruneLocked(now)
	out := make([]nowEntry, 0, len(n.byUser[userID]))
	for _, e := range n.byUser[userID] {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.After(out[j].at)
		}
		return out[i].playerID < out[j].playerID
	})
	return out
}

// pruneLocked drops reports older than the TTL, so memory does not grow with
// players that stopped. The caller holds mu.
func (n *nowPlayingStore) pruneLocked(now time.Time) {
	for u, m := range n.byUser {
		for p, e := range m {
			if now.Sub(e.at) > nowPlayingTTL {
				delete(m, p)
			}
		}
		if len(m) == 0 {
			delete(n.byUser, u)
		}
	}
}

// UpdateNowPlaying records the song a player is playing now. A new report from
// the same player replaces the old one. The username parameter, if sent, must
// be the caller's own.
func (s *State) UpdateNowPlaying(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	if u := queryFirst(r, "username"); u != "" && u != caller.Username {
		writeError(w, 50, "Not authorized to report playback for another user")
		return
	}
	song, err := repoFindSong(r.Context(), s.Pool, id)
	if err != nil {
		s.dbFailed(w, "updateNowPlaying", err)
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	s.nowPlaying.set(caller.ID, queryFirst(r, "playerId"), id, time.Now())
	writeOK(w, J{})
}

// GetNowPlaying lists what the caller's players are playing. Other users'
// reports are not shown, a deliberate difference from the Subsonic
// description, which lists every user (#147).
func (s *State) GetNowPlaying(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := time.Now()
	type live struct {
		e    nowEntry
		song models.Song
	}
	var rows []live
	var songs []models.Song
	for _, e := range s.nowPlaying.list(caller.ID, now) {
		song, err := repoFindSong(ctx, s.Pool, e.songID)
		if err != nil {
			s.dbFailed(w, "getNowPlaying", err)
			return
		}
		if song == nil {
			// The song left the library; its report is not shown.
			continue
		}
		rows = append(rows, live{e: e, song: *song})
		songs = append(songs, *song)
	}
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "getNowPlaying", err)
		return
	}
	entries := make([]J, 0, len(rows))
	for _, row := range rows {
		entry := s.songToChild(row.song, plays)
		entry["username"] = caller.Username
		entry["minutesAgo"] = int(now.Sub(row.e.at) / time.Minute)
		entry["playerId"] = row.e.playerID
		entries = append(entries, entry)
	}
	writeOK(w, J{"nowPlaying": J{"entry": entries}})
}
