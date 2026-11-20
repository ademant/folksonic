package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// repoSongLyrics reads the stored lyrics of one song.
func repoSongLyrics(ctx context.Context, db *sql.DB, songID string) (string, bool, error) {
	var text string
	err := db.QueryRowContext(ctx, "SELECT text FROM song_lyrics WHERE song_id = ?", songID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// repoLyricsByArtistTitle returns the lyrics of the one song with this artist
// and title, ignoring case. Two or more songs with the name, or a song without
// lyrics, give no lyrics: the answer must not come from a guess.
func repoLyricsByArtistTitle(ctx context.Context, db *sql.DB, artist, title string) (string, bool, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT l.text FROM songs s LEFT JOIN song_lyrics l ON l.song_id = s.id "+
			"WHERE s.artist = ? COLLATE NOCASE AND s.title = ? COLLATE NOCASE LIMIT 2", artist, title)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var texts []sql.NullString
	for rows.Next() {
		var t sql.NullString
		if err := rows.Scan(&t); err != nil {
			return "", false, err
		}
		texts = append(texts, t)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(texts) != 1 || !texts[0].Valid {
		return "", false, nil
	}
	return texts[0].String, true, nil
}

// lyricsLines splits lyrics into the lines of a structuredLyrics entry. Line
// ends are normalised; empty lines are kept, so the stanzas stay apart.
func lyricsLines(text string) []J {
	parts := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]J, 0, len(parts))
	for _, p := range parts {
		out = append(out, J{"value": p})
	}
	return out
}

// GetLyricsBySongId returns the lyrics stored for a song as one unsynchronised
// structuredLyrics entry (OpenSubsonic songLyrics, #142). Synced text is
// reported as unsynchronised, and the language is always "und". A song without
// lyrics gets an empty list.
func (s *State) GetLyricsBySongId(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()
	song, err := repoFindSong(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "getLyricsBySongId", err)
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	text, ok, err := repoSongLyrics(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "getLyricsBySongId", err)
		return
	}
	structured := []J{}
	if ok {
		structured = append(structured, J{
			"displayArtist": song.Artist,
			"displayTitle":  song.Title,
			"lang":          "und",
			"synced":        false,
			"line":          lyricsLines(text),
		})
	}
	writeOK(w, J{"lyricsList": J{"structuredLyrics": structured}})
}

// GetLyrics returns the lyrics of the song with this artist and title (#142).
// The value is left out when no single song matches.
func (s *State) GetLyrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	artist, title := queryFirst(r, "artist"), queryFirst(r, "title")
	out := J{"artist": artist, "title": title}
	if artist != "" && title != "" {
		text, ok, err := repoLyricsByArtistTitle(r.Context(), s.Pool, artist, title)
		if err != nil {
			s.dbFailed(w, "getLyrics", err)
			return
		}
		if ok {
			out["value"] = text
		}
	}
	writeOK(w, J{"lyrics": out})
}
