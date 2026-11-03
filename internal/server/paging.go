package server

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/tsirysndr/tinysonic/internal/scanner"
)

// maxPageSize caps the size parameter of the tinysonicPaging extension, like
// the standard list endpoints.
const maxPageSize = 500

// page is a size/offset window from the tinysonicPaging extension. Without
// either parameter, set is false and lists are returned whole, as before.
type page struct {
	size, offset int64
	set          bool
}

func pageParams(r *http.Request) page {
	if queryFirst(r, "size") == "" && queryFirst(r, "offset") == "" {
		return page{}
	}
	return page{
		size:   clamp(intParam(r, "size", maxPageSize), 1, maxPageSize),
		offset: max0(intParam(r, "offset", 0)),
		set:    true,
	}
}

// pageSlice applies p to an already ordered list.
func pageSlice[T any](items []T, p page) []T {
	if !p.set {
		return items
	}
	if p.offset >= int64(len(items)) {
		return items[:0]
	}
	return items[p.offset:min(p.offset+p.size, int64(len(items)))]
}

// countParam returns the endpoint's own count parameter when given, else the
// generic size alias (capped at maxPageSize), else def.
func countParam(r *http.Request, own string, def int64) int64 {
	if queryFirst(r, own) != "" {
		return intParam(r, own, def)
	}
	if queryFirst(r, "size") != "" {
		return clamp(intParam(r, "size", def), 1, maxPageSize)
	}
	return def
}

// offsetParam is countParam for offsets.
func offsetParam(r *http.Request, own string) int64 {
	if queryFirst(r, own) != "" {
		return max0(intParam(r, own, 0))
	}
	return max0(intParam(r, "offset", 0))
}

// Total-count headers. Single-list endpoints send X-Total-Count; endpoints
// with artist, album and song lists send one header per list.
const (
	hdrTotal       = "X-Total-Count"
	hdrTotalArtist = "X-Total-Count-Artist"
	hdrTotalAlbum  = "X-Total-Count-Album"
	hdrTotalSong   = "X-Total-Count-Song"
)

// exposedHeaders are readable by browser clients across origins.
const exposedHeaders = hdrTotal + ", " + hdrTotalArtist + ", " + hdrTotalAlbum + ", " + hdrTotalSong + ", Preference-Applied"

func setTotal(w http.ResponseWriter, n int) {
	w.Header().Set(hdrTotal, strconv.Itoa(n))
}

func setTotals(w http.ResponseWriter, artists, albums, songs int) {
	w.Header().Set(hdrTotalArtist, strconv.Itoa(artists))
	w.Header().Set(hdrTotalAlbum, strconv.Itoa(albums))
	w.Header().Set(hdrTotalSong, strconv.Itoa(songs))
}

// headDone finishes a HEAD request once the total headers are set, so the
// list itself is never built. It reports whether it did.
func headDone(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodHead {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	return true
}

// ── tinysonicLibraryInfo ───────────────────────────────────────────────────

// TinysonicLibraryInfo returns library totals and the time of the last
// library change, so a client can skip a sync when nothing changed.
func (s *State) TinysonicLibraryInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	info, err := repoLibraryInfo(r.Context(), s.Pool)
	if err != nil {
		writeError(w, 0, "database error")
		return
	}
	writeOK(w, J{"tinysonicLibraryInfo": info})
}

func repoLibraryInfo(ctx context.Context, db *sql.DB) (J, error) {
	var artists, albums, songs, durMs int64
	var modified sql.NullString
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM artists), (SELECT COUNT(*) FROM albums),
		(SELECT COUNT(*) FROM songs), (SELECT COALESCE(SUM(duration_ms), 0) FROM songs),
		COALESCE((SELECT value FROM settings WHERE key = ?), (SELECT MAX(created_at) FROM songs))`,
		scanner.LibraryModifiedKey).Scan(&artists, &albums, &songs, &durMs, &modified)
	if err != nil {
		return nil, err
	}
	if !modified.Valid {
		// Empty library that never changed.
		modified = sql.NullString{String: time.Unix(0, 0).UTC().Format("2006-01-02T15:04:05Z"), Valid: true}
	}
	return J{
		"artists":      artists,
		"albums":       albums,
		"songs":        songs,
		"duration":     durMs / 1000,
		"lastModified": modified.String,
	}, nil
}
