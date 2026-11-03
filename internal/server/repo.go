package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/tsirysndr/tinysonic/internal/models"
)

// ── Artists ──────────────────────────────────────────────────────────────────

func repoAllArtists(ctx context.Context, db *sql.DB) ([]models.Artist, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, name, mbid FROM artists ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Artist
	for rows.Next() {
		var a models.Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.MBID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func repoFindArtist(ctx context.Context, db *sql.DB, id string) (*models.Artist, error) {
	var a models.Artist
	err := db.QueryRowContext(ctx, "SELECT id, name, mbid FROM artists WHERE id = ?", id).Scan(&a.ID, &a.Name, &a.MBID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func repoAlbumCountsByArtist(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, "SELECT artist_id, COUNT(*) FROM albums GROUP BY artist_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ── Albums ───────────────────────────────────────────────────────────────────

const albumCols = "id, title, artist, artist_id, year, cover_art, mbid"

func scanAlbum(s rowScanner) (models.Album, error) {
	var a models.Album
	err := s.Scan(&a.ID, &a.Title, &a.Artist, &a.ArtistID, &a.Year, &a.CoverArt, &a.MBID)
	return a, err
}

func repoAlbumsByArtist(ctx context.Context, db *sql.DB, artistID string) ([]models.Album, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT "+albumCols+" FROM albums WHERE artist_id = ? ORDER BY year, title COLLATE NOCASE",
		artistID)
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

func repoFindAlbum(ctx context.Context, db *sql.DB, id string) (*models.Album, error) {
	a, err := scanAlbum(db.QueryRowContext(ctx,
		"SELECT "+albumCols+" FROM albums WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func repoAlbumsPaginated(ctx context.Context, db *sql.DB, listType string, size, offset int64) ([]models.Album, error) {
	order := "ORDER BY title COLLATE NOCASE"
	switch listType {
	case "alphabeticalByArtist":
		order = "ORDER BY artist COLLATE NOCASE, title COLLATE NOCASE"
	case "newest":
		order = "ORDER BY year DESC, title COLLATE NOCASE"
	case "random":
		order = "ORDER BY RANDOM()"
	}
	q := fmt.Sprintf("SELECT %s FROM albums %s LIMIT ? OFFSET ?", albumCols, order)
	rows, err := db.QueryContext(ctx, q, size, offset)
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

// ── Songs ────────────────────────────────────────────────────────────────────

const songCols = "id, path, title, artist, artist_id, album, album_id, genre, track_number, " +
	"disc_number, year, duration_ms, bitrate, filesize, suffix, content_type, cover_art, mbid, tags"

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSong(s rowScanner) (models.Song, error) {
	var v models.Song
	err := s.Scan(
		&v.ID, &v.Path, &v.Title, &v.Artist, &v.ArtistID, &v.Album, &v.AlbumID,
		&v.Genre, &v.TrackNumber, &v.DiscNumber, &v.Year,
		&v.DurationMs, &v.Bitrate, &v.Filesize, &v.Suffix, &v.ContentType, &v.CoverArt, &v.MBID, &v.Tags,
	)
	return v, err
}

func repoSongsByAlbum(ctx context.Context, db *sql.DB, albumID string) ([]models.Song, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT "+songCols+" FROM songs WHERE album_id = ? "+
			"ORDER BY disc_number, track_number, title COLLATE NOCASE",
		albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Song
	for rows.Next() {
		s, err := scanSong(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func repoFindSong(ctx context.Context, db *sql.DB, id string) (*models.Song, error) {
	s, err := scanSong(db.QueryRowContext(ctx,
		"SELECT "+songCols+" FROM songs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func repoSongCountForAlbum(ctx context.Context, db *sql.DB, albumID string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM songs WHERE album_id = ?", albumID).Scan(&n)
	return n, err
}

func repoSongDurationForAlbum(ctx context.Context, db *sql.DB, albumID string) (int64, error) {
	var total int64
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(duration_ms), 0) FROM songs WHERE album_id = ?", albumID).Scan(&total)
	return total / 1000, err
}

func repoRandomSongs(ctx context.Context, db *sql.DB, size int64, fromYear, toYear *int64, genre string) ([]models.Song, error) {
	q := "SELECT " + songCols + " FROM songs WHERE 1=1"
	var args []any
	if fromYear != nil {
		q += " AND year >= ?"
		args = append(args, *fromYear)
	}
	if toYear != nil {
		q += " AND year <= ?"
		args = append(args, *toYear)
	}
	if genre != "" {
		q += " AND genre = ? COLLATE NOCASE"
		args = append(args, genre)
	}
	q += " ORDER BY RANDOM() LIMIT ?"
	args = append(args, size)
	return queryManySongs(ctx, db, q, args...)
}

func repoDistinctGenres(ctx context.Context, db *sql.DB) ([]genreRow, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT genre, COUNT(*) AS song_count, "+
			"(SELECT COUNT(DISTINCT s2.album_id) FROM songs s2 WHERE s2.genre = songs.genre) AS album_count "+
			"FROM songs WHERE genre IS NOT NULL AND genre <> '' "+
			"GROUP BY genre ORDER BY genre COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []genreRow
	for rows.Next() {
		var g genreRow
		if err := rows.Scan(&g.Name, &g.SongCount, &g.AlbumCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type genreRow struct {
	Name       string
	SongCount  int64
	AlbumCount int64
}

func repoSongsByGenre(ctx context.Context, db *sql.DB, genre string, count, offset int64) ([]models.Song, error) {
	return queryManySongs(ctx, db,
		"SELECT "+songCols+" FROM songs WHERE genre = ? COLLATE NOCASE "+
			"ORDER BY artist COLLATE NOCASE, album COLLATE NOCASE, track_number "+
			"LIMIT ? OFFSET ?", genre, count, offset)
}

func queryManySongs(ctx context.Context, db *sql.DB, q string, args ...any) ([]models.Song, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Song
	for rows.Next() {
		s, err := scanSong(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── FTS search ───────────────────────────────────────────────────────────────

// fts5Match converts user input into an FTS5 MATCH expression: each
// whitespace-separated token becomes a quoted prefix term, ANDed together.
// Returns "" if no usable tokens remain.
func fts5Match(term string) string {
	parts := strings.Fields(term)
	var clean []string
	for _, w := range parts {
		var b strings.Builder
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			clean = append(clean, `"`+b.String()+`"*`)
		}
	}
	return strings.Join(clean, " ")
}

func repoSearchArtists(ctx context.Context, db *sql.DB, term string, limit, offset int64) ([]models.Artist, error) {
	q := fts5Match(term)
	if q == "" {
		rows, err := db.QueryContext(ctx,
			"SELECT id, name FROM artists ORDER BY name COLLATE NOCASE LIMIT ? OFFSET ?",
			limit, offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []models.Artist
		for rows.Next() {
			var a models.Artist
			if err := rows.Scan(&a.ID, &a.Name, &a.MBID); err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, rows.Err()
	}
	rows, err := db.QueryContext(ctx,
		"SELECT a.id, a.name, a.mbid FROM artists_fts f INNER JOIN artists a ON a.id = f.id "+
			"WHERE f.artists_fts MATCH ? ORDER BY f.rank LIMIT ? OFFSET ?",
		q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Artist
	for rows.Next() {
		var a models.Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.MBID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func repoSearchAlbums(ctx context.Context, db *sql.DB, term string, limit, offset int64) ([]models.Album, error) {
	q := fts5Match(term)
	if q == "" {
		return queryManyAlbums(ctx, db,
			"SELECT "+albumCols+" FROM albums ORDER BY title COLLATE NOCASE LIMIT ? OFFSET ?",
			limit, offset)
	}
	return queryManyAlbums(ctx, db,
		"SELECT a.id, a.title, a.artist, a.artist_id, a.year, a.cover_art, a.mbid "+
			"FROM albums_fts f INNER JOIN albums a ON a.id = f.id "+
			"WHERE f.albums_fts MATCH ? ORDER BY f.rank LIMIT ? OFFSET ?",
		q, limit, offset)
}

func queryManyAlbums(ctx context.Context, db *sql.DB, q string, args ...any) ([]models.Album, error) {
	rows, err := db.QueryContext(ctx, q, args...)
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

func repoSearchSongs(ctx context.Context, db *sql.DB, term string, limit, offset int64) ([]models.Song, error) {
	q := fts5Match(term)
	if q == "" {
		return queryManySongs(ctx, db,
			"SELECT "+songCols+" FROM songs ORDER BY title COLLATE NOCASE LIMIT ? OFFSET ?",
			limit, offset)
	}
	return queryManySongs(ctx, db,
		"SELECT s.id, s.path, s.title, s.artist, s.artist_id, s.album, s.album_id, s.genre, "+
			"s.track_number, s.disc_number, s.year, s.duration_ms, s.bitrate, s.filesize, "+
			"s.suffix, s.content_type, s.cover_art, s.mbid, s.tags "+
			"FROM songs_fts f INNER JOIN songs s ON s.id = f.id "+
			"WHERE f.songs_fts MATCH ? ORDER BY f.rank LIMIT ? OFFSET ?",
		q, limit, offset)
}

// ── Starred ─────────────────────────────────────────────────────────────────

func repoStar(ctx context.Context, db *sql.DB, userID int64, id, when string) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO starred (user_id, id, starred_at) VALUES (?, ?, ?) "+
			"ON CONFLICT(user_id, id) DO UPDATE SET starred_at = excluded.starred_at",
		userID, id, when)
	return err
}

func repoUnstar(ctx context.Context, db *sql.DB, userID int64, id string) error {
	_, err := db.ExecContext(ctx, "DELETE FROM starred WHERE user_id = ? AND id = ?", userID, id)
	return err
}

type starredSong struct {
	Song models.Song
	When string
}
type starredAlbum struct {
	Album models.Album
	When  string
}
type starredArtist struct {
	Artist models.Artist
	When   string
}

func repoStarredSongs(ctx context.Context, db *sql.DB, userID int64) ([]starredSong, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT st.id, st.starred_at FROM starred st INNER JOIN songs s ON s.id = st.id "+
			"WHERE st.user_id = ? ORDER BY st.starred_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct{ id, when string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.when); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []starredSong
	for _, p := range pairs {
		s, err := repoFindSong(ctx, db, p.id)
		if err != nil || s == nil {
			continue
		}
		out = append(out, starredSong{Song: *s, When: p.when})
	}
	return out, nil
}

func repoStarredAlbums(ctx context.Context, db *sql.DB, userID int64) ([]starredAlbum, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT st.id, st.starred_at FROM starred st INNER JOIN albums a ON a.id = st.id "+
			"WHERE st.user_id = ? ORDER BY st.starred_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct{ id, when string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.when); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []starredAlbum
	for _, p := range pairs {
		a, err := repoFindAlbum(ctx, db, p.id)
		if err != nil || a == nil {
			continue
		}
		out = append(out, starredAlbum{Album: *a, When: p.when})
	}
	return out, nil
}

func repoStarredArtists(ctx context.Context, db *sql.DB, userID int64) ([]starredArtist, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT st.id, st.starred_at FROM starred st INNER JOIN artists ar ON ar.id = st.id "+
			"WHERE st.user_id = ? ORDER BY st.starred_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct{ id, when string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.when); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []starredArtist
	for _, p := range pairs {
		a, err := repoFindArtist(ctx, db, p.id)
		if err != nil || a == nil {
			continue
		}
		out = append(out, starredArtist{Artist: *a, When: p.when})
	}
	return out, nil
}

// ── Playlists ───────────────────────────────────────────────────────────────

func repoAllPlaylists(ctx context.Context, db *sql.DB, ownerID int64) ([]models.Playlist, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, name, comment, public, created_at, updated_at FROM playlists "+
			"WHERE owner_id = ? ORDER BY name COLLATE NOCASE", ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Playlist
	for rows.Next() {
		var p models.Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Comment, &p.Public, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// repoFindPlaylist returns the playlist only if it belongs to ownerID, so a
// playlist id from another account reads as not found.
func repoFindPlaylist(ctx context.Context, db *sql.DB, ownerID int64, id string) (*models.Playlist, error) {
	var p models.Playlist
	err := db.QueryRowContext(ctx,
		"SELECT id, name, comment, public, created_at, updated_at FROM playlists WHERE id = ? AND owner_id = ?",
		id, ownerID).
		Scan(&p.ID, &p.Name, &p.Comment, &p.Public, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func repoCreatePlaylist(ctx context.Context, db *sql.DB, ownerID int64, id, name, now string) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO playlists (id, name, comment, public, created_at, updated_at, owner_id) "+
			"VALUES (?, ?, NULL, 1, ?, ?, ?)",
		id, name, now, now, ownerID)
	return err
}

func repoRenamePlaylist(ctx context.Context, db *sql.DB, id, name, now string) error {
	_, err := db.ExecContext(ctx,
		"UPDATE playlists SET name = ?, updated_at = ? WHERE id = ?", name, now, id)
	return err
}

func repoSetPlaylistComment(ctx context.Context, db *sql.DB, id, comment, now string) error {
	_, err := db.ExecContext(ctx,
		"UPDATE playlists SET comment = ?, updated_at = ? WHERE id = ?", comment, now, id)
	return err
}

func repoDeletePlaylist(ctx context.Context, db *sql.DB, ownerID int64, id string) error {
	res, err := db.ExecContext(ctx, "DELETE FROM playlists WHERE id = ? AND owner_id = ?", id, ownerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	_, err = db.ExecContext(ctx, "DELETE FROM playlist_songs WHERE playlist_id = ?", id)
	return err
}

func repoPlaylistSongIDs(ctx context.Context, db *sql.DB, playlistID string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT song_id FROM playlist_songs WHERE playlist_id = ? ORDER BY position", playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func repoPlaylistSongs(ctx context.Context, db *sql.DB, playlistID string) ([]models.Song, error) {
	return queryManySongs(ctx, db,
		"SELECT s.id, s.path, s.title, s.artist, s.artist_id, s.album, s.album_id, s.genre, "+
			"s.track_number, s.disc_number, s.year, s.duration_ms, s.bitrate, s.filesize, "+
			"s.suffix, s.content_type, s.cover_art, s.mbid, s.tags "+
			"FROM playlist_songs ps INNER JOIN songs s ON s.id = ps.song_id "+
			"WHERE ps.playlist_id = ? ORDER BY ps.position", playlistID)
}

func repoAppendPlaylistSongs(ctx context.Context, db *sql.DB, playlistID string, songIDs []string) error {
	if len(songIDs) == 0 {
		return nil
	}
	var next int64
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(position), -1) + 1 FROM playlist_songs WHERE playlist_id = ?",
		playlistID).Scan(&next); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, sid := range songIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO playlist_songs (playlist_id, position, song_id) VALUES (?, ?, ?)",
			playlistID, next+int64(i), sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func repoReplacePlaylistSongs(ctx context.Context, db *sql.DB, playlistID string, songIDs []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM playlist_songs WHERE playlist_id = ?", playlistID); err != nil {
		return err
	}
	for i, sid := range songIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO playlist_songs (playlist_id, position, song_id) VALUES (?, ?, ?)",
			playlistID, int64(i), sid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
