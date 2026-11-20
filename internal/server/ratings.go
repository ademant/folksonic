package server

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// Personal star ratings (#140). A rating is one user's preference for a song,
// 0 to 5 stars, kept apart from stars and from the dancing shoes values, which
// stay on DanceBrainz. Songs carry it as userRating for the caller only.

// SetRating stores the caller's rating of a song. A rating of 0 removes it.
func (s *State) SetRating(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	rating, err := strconv.Atoi(queryFirst(r, "rating"))
	if err != nil || rating < 0 || rating > 5 {
		writeError(w, 10, "Invalid parameter: rating must be a whole number from 0 to 5")
		return
	}
	ctx := r.Context()
	song, err := repoFindSong(ctx, s.Pool, id)
	if err != nil {
		logging.Errorf("setRating lookup %s: %v", id, err)
		writeError(w, 0, "database error")
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	if err := repoSetRating(ctx, s.Pool, caller.ID, id, rating); err != nil {
		logging.Errorf("setRating %s: %v", id, err)
		writeError(w, 0, "database error")
		return
	}
	writeOK(w, J{})
}

// repoSetRating stores a rating from 1 to 5, or removes the caller's rating for 0.
func repoSetRating(ctx context.Context, db *sql.DB, userID int64, songID string, rating int) error {
	if rating == 0 {
		_, err := db.ExecContext(ctx, "DELETE FROM user_ratings WHERE user_id = ? AND song_id = ?", userID, songID)
		return err
	}
	_, err := db.ExecContext(ctx,
		"INSERT INTO user_ratings (user_id, song_id, rating) VALUES (?, ?, ?) "+
			"ON CONFLICT(user_id, song_id) DO UPDATE SET rating = excluded.rating",
		userID, songID, rating)
	return err
}
