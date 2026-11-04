package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/models"
)

// changeSettleMs is how far serverTime lags the clock. A write whose
// transaction commits just after a response was built has a timestamp inside
// this window, so the next sync, which starts from serverTime, still finds it.
// Tests set it to 0.
var changeSettleMs int64 = 2000

// changeCursor is the position in one change-feed snapshot. It is opaque to
// clients. Snap is the snapshot's serverTime, Since the sync point it answers,
// and (Ms, Kind, ID) the last row sent.
type changeCursor struct {
	Snap, Since int64
	Full        bool
	Ms          int64
	Kind, ID    string
}

func (c changeCursor) encode() string {
	raw := fmt.Sprintf("%d|%d|%t|%d|%s|%s", c.Snap, c.Since, c.Full, c.Ms, c.Kind, c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeChangeCursor(s string) (changeCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return changeCursor{}, err
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 6 {
		return changeCursor{}, fmt.Errorf("cursor has %d fields", len(f))
	}
	var c changeCursor
	if c.Snap, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return changeCursor{}, err
	}
	if c.Since, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return changeCursor{}, err
	}
	if c.Full, err = strconv.ParseBool(f[2]); err != nil {
		return changeCursor{}, err
	}
	if c.Ms, err = strconv.ParseInt(f[3], 10, 64); err != nil {
		return changeCursor{}, err
	}
	c.Kind, c.ID = f[4], f[5]
	return c, nil
}

// changeRow is one line of the merged change stream. Kind is one of artist,
// album, song, playlist, starred or deleted; T is the item type for starred
// and deleted rows.
type changeRow struct {
	Kind, ID, T string
	Ms          int64
}

// changeRows returns up to limit rows after the cursor, in (ms, kind, id)
// order, from the snapshot the cursor names. limit -1 means no limit.
// Playlists and starred items are limited to userID. Deletions are included
// only for incremental responses.
func changeRows(ctx context.Context, d *sql.DB, userID int64, c changeCursor, limit int64) ([]changeRow, error) {
	var parts []string
	var args []any
	add := func(q string, a ...any) {
		parts = append(parts, q)
		args = append(args, a...)
	}
	since, snap := c.Since, c.Snap

	// The first branch names the columns the outer query uses.
	add("SELECT 'artist' AS k, id, updated_ms AS ms, 'artist' AS t FROM artists WHERE updated_ms > ? AND updated_ms <= ?", since, snap)
	add("SELECT 'album', id, updated_ms, 'album' FROM albums WHERE updated_ms > ? AND updated_ms <= ?", since, snap)
	add("SELECT 'song', id, updated_ms, 'song' FROM songs WHERE updated_ms > ? AND updated_ms <= ?", since, snap)
	add("SELECT 'playlist', id, updated_ms, 'playlist' FROM playlists WHERE owner_id = ? AND updated_ms > ? AND updated_ms <= ?",
		userID, since, snap)
	add(`SELECT 'starred', id, updated_ms,
	        CASE WHEN EXISTS (SELECT 1 FROM songs s WHERE s.id = starred.id) THEN 'song'
	             WHEN EXISTS (SELECT 1 FROM albums a WHERE a.id = starred.id) THEN 'album'
	             ELSE 'artist' END
	     FROM starred WHERE user_id = ? AND updated_ms > ? AND updated_ms <= ?`,
		userID, since, snap)
	if !c.Full {
		add("SELECT 'deleted', item_id, deleted_ms, kind FROM change_tombstones "+
			"WHERE deleted_ms > ? AND deleted_ms <= ? AND (user_id IS NULL OR user_id = ?)",
			since, snap, userID)
	}

	q := "SELECT k, id, ms, t FROM (" + strings.Join(parts, " UNION ALL ") + ") " +
		"WHERE (ms > ? OR (ms = ? AND (k > ? OR (k = ? AND id > ?)))) " +
		"ORDER BY ms, k, id LIMIT ?"
	args = append(args, c.Ms, c.Ms, c.Kind, c.Kind, c.ID, limit)

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.Kind, &r.ID, &r.Ms, &r.T); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLibraryChanges answers getLibraryChanges, the tinysonicLibraryChanges
// extension. It lists what changed since the sync point `since`, which is the
// serverTime of the previous answer. Without `since`, or when `since` is older
// than the floor, it returns the full library and sets full. With `size`, the
// answer is split into pages linked by nextCursor. Every page of one snapshot
// reports the same serverTime.
//
// With include=items (version 2 of the extension), artist, album and song
// entries also carry the item itself, as getArtist (without albums),
// getAlbum (without songs) and getSong return it, and the answer is paged
// at maxPageSize entries when size is not given.
func (s *State) GetLibraryChanges(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	withItems := false
	switch v := queryFirst(r, "include"); v {
	case "":
	case "items":
		withItems = true
	default:
		writeError(w, 0, "include must be items")
		return
	}

	var c changeCursor
	if cur := queryFirst(r, "cursor"); cur != "" {
		parsed, err := decodeChangeCursor(cur)
		if err != nil {
			writeError(w, 0, "cursor is not valid")
			return
		}
		c = parsed
	} else {
		since, err := changeSinceParam(r)
		if err != nil {
			writeError(w, 0, "since must be epoch milliseconds")
			return
		}
		floor, err := db.ChangeFloor(ctx, s.Pool)
		if err != nil {
			writeError(w, 0, err.Error())
			return
		}
		full := since == 0 || since < floor
		if full {
			since = 0
		}
		c = changeCursor{
			Snap:  time.Now().UnixMilli() - changeSettleMs,
			Since: since,
			Full:  full,
			Ms:    since,
		}
	}

	p := pageParams(r)
	if withItems && !p.set {
		// Full objects are large; never build the whole library at once.
		p = page{size: maxPageSize, set: true}
	}
	limit := int64(-1)
	if p.set {
		limit = p.size + 1
	}
	rows, err := changeRows(ctx, s.Pool, caller.ID, c, limit)
	if err != nil {
		writeError(w, 0, err.Error())
		return
	}

	var next string
	if p.set && int64(len(rows)) > p.size {
		rows = rows[:p.size]
		last := rows[len(rows)-1]
		nc := c
		nc.Ms, nc.Kind, nc.ID = last.Ms, last.Kind, last.ID
		next = nc.encode()
	}

	var items changeItemSet
	if withItems {
		items, err = s.changeItems(ctx, rows)
		if err != nil {
			writeError(w, 0, err.Error())
			return
		}
	}
	// entry is the {id, updated} of a library item, plus the item itself when
	// asked for and still present (it may have been deleted since the
	// snapshot; the next sync then reports the deletion).
	entry := func(r changeRow, objs map[string]J) J {
		e := J{"id": r.ID, "updated": r.Ms}
		if obj, ok := objs[r.ID]; ok {
			e["item"] = obj
		}
		return e
	}

	artists, albums, songs := []J{}, []J{}, []J{}
	playlists, starred, deleted := []J{}, []J{}, []J{}
	for _, r := range rows {
		switch r.Kind {
		case "artist":
			artists = append(artists, entry(r, items.artists))
		case "album":
			albums = append(albums, entry(r, items.albums))
		case "song":
			songs = append(songs, entry(r, items.songs))
		case "playlist":
			playlists = append(playlists, J{"id": r.ID, "updated": r.Ms})
		case "starred":
			starred = append(starred, J{"id": r.ID, "type": r.T, "updated": r.Ms})
		case "deleted":
			deleted = append(deleted, J{"type": r.T, "id": r.ID, "deleted": r.Ms})
		}
	}

	out := J{
		"full":       c.Full,
		"serverTime": c.Snap,
		"artists":    artists,
		"albums":     albums,
		"songs":      songs,
		"playlists":  playlists,
		"starred":    starred,
		"deleted":    deleted,
	}
	if next != "" {
		out["nextCursor"] = next
	}
	writeOK(w, J{"libraryChanges": out})
}

// changeItemSet holds the objects for one page of include=items, by id.
type changeItemSet struct {
	artists, albums, songs map[string]J
}

// changeItems loads the artists, albums and songs of a page, one query per
// kind. A page has at most maxPageSize rows, well inside SQLite's limit on
// bound parameters.
func (s *State) changeItems(ctx context.Context, rows []changeRow) (changeItemSet, error) {
	var artistIDs, albumIDs, songIDs []any
	for _, r := range rows {
		switch r.Kind {
		case "artist":
			artistIDs = append(artistIDs, r.ID)
		case "album":
			albumIDs = append(albumIDs, r.ID)
		case "song":
			songIDs = append(songIDs, r.ID)
		}
	}
	set := changeItemSet{artists: map[string]J{}, albums: map[string]J{}, songs: map[string]J{}}

	if len(artistIDs) > 0 {
		q := "SELECT a.id, a.name, a.mbid, (SELECT COUNT(*) FROM albums WHERE artist_id = a.id) " +
			"FROM artists a WHERE a.id IN (" + placeholders(len(artistIDs)) + ")"
		rs, err := s.Pool.QueryContext(ctx, q, artistIDs...)
		if err != nil {
			return set, err
		}
		defer rs.Close()
		for rs.Next() {
			var a models.Artist
			var n int64
			if err := rs.Scan(&a.ID, &a.Name, &a.MBID, &n); err != nil {
				return set, err
			}
			set.artists[a.ID] = artistToJSON(a, n)
		}
		if err := rs.Err(); err != nil {
			return set, err
		}
	}
	if len(albumIDs) > 0 {
		as, err := queryManyAlbums(ctx, s.Pool,
			"SELECT "+albumCols+" FROM albums a WHERE a.id IN ("+placeholders(len(albumIDs))+")", albumIDs...)
		if err != nil {
			return set, err
		}
		for _, a := range as {
			set.albums[a.ID] = albumToChild(a)
		}
	}
	if len(songIDs) > 0 {
		ss, err := queryManySongs(ctx, s.Pool,
			"SELECT "+songCols+" FROM songs s WHERE s.id IN ("+placeholders(len(songIDs))+")", songIDs...)
		if err != nil {
			return set, err
		}
		for _, so := range ss {
			set.songs[so.ID] = s.songToChild(so)
		}
	}
	return set, nil
}

// placeholders returns n comma-separated SQL parameters.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// changeSinceParam reads `since`. It is 0 when absent.
func changeSinceParam(r *http.Request) (int64, error) {
	v := queryFirst(r, "since")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("since %q", v)
	}
	return n, nil
}
