package server

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// maxBookmarkComment is the longest comment a bookmark keeps (#139).
const maxBookmarkComment = 256

// headScanner reads the leading columns of a row into head and the rest into
// the caller's destinations, so one Scan fills a bookmark and its song.
type headScanner struct {
	rows *sql.Rows
	head []any
}

func (h headScanner) Scan(dest ...any) error {
	return h.rows.Scan(append(append([]any{}, h.head...), dest...)...)
}

// isoTime is a stored ms timestamp in the form the other responses use.
func isoTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

// CreateBookmark sets the caller's bookmark for a song, replacing the old
// position and comment (#139). The creation time of an existing bookmark is
// kept.
func (s *State) CreateBookmark(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	raw := queryFirst(r, "position")
	if raw == "" {
		writeError(w, 10, "Required parameter is missing: position")
		return
	}
	position, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || position < 0 {
		writeError(w, 10, "Invalid parameter: position")
		return
	}
	comment := queryFirst(r, "comment")
	if utf8.RuneCountInString(comment) > maxBookmarkComment {
		writeError(w, 10, "Invalid parameter: comment is longer than 256 characters")
		return
	}
	ctx := r.Context()
	song, err := repoFindSong(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "createBookmark", err)
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	now := time.Now().UnixMilli()
	if _, err := s.Pool.ExecContext(ctx,
		"INSERT INTO bookmarks (user_id, song_id, position_ms, comment, created_ms, changed_ms) "+
			"VALUES (?, ?, ?, ?, ?, ?) "+
			"ON CONFLICT(user_id, song_id) DO UPDATE SET "+
			"position_ms = excluded.position_ms, comment = excluded.comment, changed_ms = excluded.changed_ms",
		caller.ID, id, position, comment, now, now); err != nil {
		s.dbFailed(w, "createBookmark", err)
		return
	}
	writeOK(w, J{})
}

// DeleteBookmark removes the caller's bookmark for a song. Other users'
// bookmarks are not affected.
func (s *State) DeleteBookmark(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	if _, err := s.Pool.ExecContext(r.Context(),
		"DELETE FROM bookmarks WHERE user_id = ? AND song_id = ?", caller.ID, id); err != nil {
		s.dbFailed(w, "deleteBookmark", err)
		return
	}
	writeOK(w, J{})
}

// GetBookmarks lists the caller's bookmarks. A bookmark whose song has left
// the library is kept but not returned.
func (s *State) GetBookmarks(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rows, err := s.Pool.QueryContext(ctx,
		"SELECT b.position_ms, b.comment, b.created_ms, b.changed_ms, "+songCols+
			" FROM bookmarks b JOIN songs s ON s.id = b.song_id "+
			"WHERE b.user_id = ? ORDER BY b.changed_ms DESC, s.id", caller.ID)
	if err != nil {
		s.dbFailed(w, "getBookmarks", err)
		return
	}
	defer rows.Close()
	type bookmark struct {
		position, created, changed int64
		comment                    string
		song                       models.Song
	}
	var found []bookmark
	var songs []models.Song
	for rows.Next() {
		var b bookmark
		head := []any{&b.position, &b.comment, &b.created, &b.changed}
		if b.song, err = scanSong(headScanner{rows: rows, head: head}); err != nil {
			s.dbFailed(w, "getBookmarks", err)
			return
		}
		found = append(found, b)
		songs = append(songs, b.song)
	}
	if err := rows.Err(); err != nil {
		s.dbFailed(w, "getBookmarks", err)
		return
	}
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "getBookmarks", err)
		return
	}
	out := make([]J, 0, len(found))
	for _, b := range found {
		entry := s.songToChild(b.song, plays)
		entry["bookmarkPosition"] = b.position
		out = append(out, J{
			"entry":    entry,
			"position": b.position,
			"username": caller.Username,
			"comment":  b.comment,
			"created":  isoTime(b.created),
			"changed":  isoTime(b.changed),
		})
	}
	writeOK(w, J{"bookmarks": J{"bookmark": out}})
}
