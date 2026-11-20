package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// badParam is a request error: the parameters do not describe a list. It is
// answered with Subsonic error 10.
type badParam string

func (b badParam) Error() string { return string(b) }

// albumQuery is one getAlbumList2 list: where it reads from, its filter, the
// arguments of both, and its order. The count and the page use the same parts.
type albumQuery struct {
	from  string
	where string
	args  []any
	order string
}

// albumListQuery builds the list for listType. frequent and recent use the
// caller's own play counts (#137); byYear and byGenre filter albums (#144).
// highest and starred are not implemented and keep the alphabetical order they
// have always had, so clients that send them still get albums.
func albumListQuery(listType string, userID int64, fromYear, toYear *int64, genre string) (albumQuery, error) {
	switch listType {
	case "frequent", "recent":
		order := "ORDER BY p.plays DESC, a.id"
		if listType == "recent" {
			order = "ORDER BY p.last_ms DESC, a.id"
		}
		return albumQuery{
			from: "albums a JOIN (SELECT s.album_id AS album_id, SUM(pc.play_count) AS plays, " +
				"MAX(pc.last_played_ms) AS last_ms FROM play_counts pc JOIN songs s ON s.id = pc.song_id " +
				"WHERE pc.user_id = ? GROUP BY s.album_id) p ON p.album_id = a.id",
			args:  []any{userID},
			order: order,
		}, nil
	case "byYear":
		if fromYear == nil || toYear == nil {
			return albumQuery{}, badParam("Required parameter is missing: fromYear and toYear")
		}
		lo, hi, dir := *fromYear, *toYear, "ASC"
		if lo > hi {
			// A reversed range is a reverse chronological list (Subsonic).
			lo, hi, dir = hi, lo, "DESC"
		}
		return albumQuery{
			from:  "albums a",
			where: " WHERE a.year > 0 AND a.year BETWEEN ? AND ?",
			args:  []any{lo, hi},
			order: "ORDER BY a.year " + dir + ", a.title COLLATE NOCASE, a.id",
		}, nil
	case "byGenre":
		if genre == "" {
			return albumQuery{}, badParam("Required parameter is missing: genre")
		}
		return albumQuery{
			from:  "albums a",
			where: " WHERE EXISTS (SELECT 1 FROM songs s WHERE s.album_id = a.id AND s.genre = ? COLLATE NOCASE)",
			args:  []any{genre},
			order: "ORDER BY a.title COLLATE NOCASE, a.id",
		}, nil
	case "alphabeticalByName", "alphabeticalByArtist", "newest", "random", "highest", "starred":
		order := "ORDER BY a.title COLLATE NOCASE, a.id"
		switch listType {
		case "alphabeticalByArtist":
			order = "ORDER BY a.artist COLLATE NOCASE, a.title COLLATE NOCASE, a.id"
		case "newest":
			// Subsonic "newest" means recently added, not release year.
			order = "ORDER BY a_created DESC, a.id"
		case "random":
			order = "ORDER BY RANDOM()"
		}
		return albumQuery{from: "albums a", order: order}, nil
	}
	return albumQuery{}, badParam("Unknown album list type: " + listType)
}

// repoAlbumListCount counts every album in the list.
func repoAlbumListCount(ctx context.Context, db *sql.DB, q albumQuery) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q.from+q.where, q.args...).Scan(&n)
	return n, err
}

// repoAlbumListPage reads one page of the list.
func repoAlbumListPage(ctx context.Context, db *sql.DB, q albumQuery, size, offset int64) ([]models.Album, error) {
	args := append(append([]any{}, q.args...), size, offset)
	rows, err := db.QueryContext(ctx,
		"SELECT "+albumCols+" FROM "+q.from+q.where+" "+q.order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Album
	for rows.Next() {
		a, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// albumList answers getAlbumList and getAlbumList2; wrap is the envelope key.
func (s *State) albumList(w http.ResponseWriter, r *http.Request, wrap string) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	listType := queryFirst(r, "type")
	if listType == "" {
		listType = "alphabeticalByName"
	}
	size := clamp(intParam(r, "size", 10), 1, 500)
	offset := max0(intParam(r, "offset", 0))
	q, err := albumListQuery(listType, caller.ID, intParamPtr(r, "fromYear"), intParamPtr(r, "toYear"), queryFirst(r, "genre"))
	var bp badParam
	if errors.As(err, &bp) {
		writeError(w, 10, string(bp))
		return
	}
	ctx := r.Context()
	total, err := repoAlbumListCount(ctx, s.Pool, q)
	if err != nil {
		s.dbFailed(w, "albumList", err)
		return
	}
	setTotal(w, int(total))
	if headDone(w, r) {
		return
	}
	albums, err := repoAlbumListPage(ctx, s.Pool, q, size, offset)
	if err != nil {
		s.dbFailed(w, "albumList", err)
		return
	}
	out := make([]J, 0, len(albums))
	for _, a := range albums {
		out = append(out, albumToChild(a))
	}
	writeOK(w, J{wrap: J{"album": out}})
}
