package server

import (
	"net/http"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// similarTempoWindow is the tempo tolerance for getSimilarSongs: a song is
// similar when its BPM is within this fraction of the seed's (#145).
const similarTempoWindow = 0.03

// GetTopSongs returns the artist's songs the caller has played most, by play
// count and then by the last play (#146). Songs never played are left out. The
// old search order is not used, since it says nothing about "top".
func (s *State) GetTopSongs(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	artist := queryFirst(r, "artist")
	if artist == "" {
		writeError(w, 10, "Required parameter is missing: artist")
		return
	}
	count := clamp(intParam(r, "count", 50), 1, 500)
	ctx := r.Context()
	songs, err := queryManySongs(ctx, s.Pool,
		"SELECT "+songCols+" FROM songs s JOIN play_counts pc ON pc.song_id = s.id AND pc.user_id = ? "+
			"WHERE s.artist = ? COLLATE NOCASE "+
			"ORDER BY pc.play_count DESC, pc.last_played_ms DESC, s.id LIMIT ?",
		caller.ID, artist, count)
	if err != nil {
		s.dbFailed(w, "GetTopSongs", err)
		return
	}
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "GetTopSongs", err)
		return
	}
	writeOK(w, J{"topSongs": J{"song": s.songsJSON(songs, plays)}})
}

// GetSimilarSongs and GetSimilarSongs2 share one answer: songs at about the
// seed's tempo (#145). Both take a song id; without a known BPM on the seed
// the answer is empty rather than a guess.
func (s *State) GetSimilarSongs(w http.ResponseWriter, r *http.Request) {
	s.similarSongs(w, r, "similarSongs")
}

func (s *State) GetSimilarSongs2(w http.ResponseWriter, r *http.Request) {
	s.similarSongs(w, r, "similarSongs2")
}

func (s *State) similarSongs(w http.ResponseWriter, r *http.Request, wrap string) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	count := clamp(intParam(r, "count", 50), 1, 500)
	ctx := r.Context()
	seed, err := repoFindSong(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, wrap, err)
		return
	}
	if seed == nil {
		writeError(w, 70, "Song not found")
		return
	}
	songs := []models.Song{}
	if seed.BPM.Valid {
		bpm := seed.BPM.Float64
		genre := ""
		if seed.Genre.Valid {
			genre = seed.Genre.String
		}
		songs, err = queryManySongs(ctx, s.Pool,
			"SELECT "+songCols+" FROM songs s WHERE s.bpm IS NOT NULL AND s.id <> ? "+
				"AND s.bpm BETWEEN ? AND ? "+
				// Nearest tempo first; among equal tempos a song of the seed's genre first.
				"ORDER BY ABS(s.bpm - ?) ASC, "+
				"CASE WHEN ? <> '' AND s.genre = ? COLLATE NOCASE THEN 0 ELSE 1 END, s.id LIMIT ?",
			id, bpm*(1-similarTempoWindow), bpm*(1+similarTempoWindow), bpm, genre, genre, count)
		if err != nil {
			s.dbFailed(w, wrap, err)
			return
		}
	}
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, wrap, err)
		return
	}
	writeOK(w, J{wrap: J{"song": s.songsJSON(songs, plays)}})
}

// songsJSON converts songs to Child objects for one caller's plays.
func (s *State) songsJSON(songs []models.Song, plays playStats) []J {
	out := make([]J, 0, len(songs))
	for _, sg := range songs {
		out = append(out, s.songToChild(sg, plays))
	}
	return out
}
