package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// duplicatesRetryAfter is the Retry-After (seconds) while the first search runs.
const duplicatesRetryAfter = 10

// duplicateIndex holds the last duplicate search. It is recomputed in the
// background (at startup and after analysis jobs that changed values), never
// in a request.
type duplicateIndex struct {
	mu           sync.Mutex
	groups       []analysis.DuplicateGroup
	computedAt   string
	fingerprints int
	running      bool
	dirty        bool // a refresh was asked for while one was running
}

// refreshDuplicates starts a duplicate search unless one is running, in
// which case that one runs once more when it ends.
func (s *State) refreshDuplicates() {
	d := &s.dups
	d.mu.Lock()
	if d.running {
		d.dirty = true
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	go func() {
		for {
			groups, n, err := s.findDuplicates(context.Background())
			d.mu.Lock()
			if err != nil {
				logging.Errorf("duplicates: %v", err)
			} else {
				d.groups, d.fingerprints, d.computedAt = groups, n, nowISO8601()
			}
			if !d.dirty {
				d.running = false
				d.mu.Unlock()
				return
			}
			d.dirty = false
			d.mu.Unlock()
		}
	}()
}

// findDuplicates loads the stored fingerprints and groups matching ones.
func (s *State) findDuplicates(ctx context.Context) ([]analysis.DuplicateGroup, int, error) {
	start := time.Now()
	rows, err := s.Pool.QueryContext(ctx, "SELECT song_id, value FROM song_values WHERE name = 'fingerprint' ORDER BY song_id")
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var tracks []analysis.Track
	bad := 0
	for rows.Next() {
		var id, value string
		if err := rows.Scan(&id, &value); err != nil {
			return nil, 0, err
		}
		var fp analysis.Fingerprint
		if json.Unmarshal([]byte(value), &fp) != nil {
			bad++
			continue
		}
		_, items, err := analysis.DecodeFingerprint(fp.Value)
		if err != nil {
			bad++
			continue
		}
		tracks = append(tracks, analysis.Track{SongID: id, Kind: fp.Algorithm + "/" + strconv.Itoa(fp.Version), Items: items})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	groups := analysis.FindDuplicates(tracks)
	logging.Infof("duplicates: %d groups among %d fingerprints (%d unreadable) in %d ms",
		len(groups), len(tracks), bad, time.Since(start).Milliseconds())
	return groups, len(tracks), nil
}

// APIDuplicates lists groups of songs that hold the same recording, by
// audio fingerprint: GET /api/duplicates. The groups come from the last
// background search; songs removed since then are left out.
func (s *State) APIDuplicates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	d := &s.dups
	d.mu.Lock()
	groups, computedAt, n, running := d.groups, d.computedAt, d.fingerprints, d.running
	d.mu.Unlock()
	if computedAt == "" {
		if !running {
			s.refreshDuplicates()
		}
		w.Header().Set("Retry-After", strconv.Itoa(duplicatesRetryAfter))
		writeJSON(w, http.StatusAccepted, J{"groups": []any{}, "computedAt": nil, "fingerprints": nil})
		return
	}

	var ids []string
	for _, g := range groups {
		ids = append(ids, g.SongIDs...)
	}
	present := map[string]bool{}
	if len(ids) > 0 {
		js, _ := json.Marshal(ids)
		rows, err := s.Pool.QueryContext(r.Context(), "SELECT id FROM songs WHERE id IN (SELECT value FROM json_each(?))", string(js))
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				present[id] = true
			}
		}
	}
	out := []analysis.DuplicateGroup{}
	for _, g := range groups {
		var keep []string
		for _, id := range g.SongIDs {
			if present[id] {
				keep = append(keep, id)
			}
		}
		if len(keep) >= 2 {
			out = append(out, analysis.DuplicateGroup{SongIDs: keep, Similarity: g.Similarity})
		}
	}
	writeJSON(w, http.StatusOK, J{"groups": out, "computedAt": computedAt, "fingerprints": n})
}
