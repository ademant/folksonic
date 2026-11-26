package scanner

import (
	"database/sql"
	"strings"
)

// maxLyricsBytes bounds the lyrics stored for one song (#198). Real lyrics,
// synced or not, are a few kilobytes; a larger tag is not lyrics.
const maxLyricsBytes = 256 << 10

// lyricsValue is the lyrics of a file as the tags carry them, or an invalid
// NullString when there are none or the text is over maxLyricsBytes. The text
// is kept as it is in the tag, so a synced (LRC) text stays in its
// timestamped form (#142).
func lyricsValue(text string) sql.NullString {
	if strings.TrimSpace(text) == "" || len(text) > maxLyricsBytes {
		return sql.NullString{}
	}
	return sql.NullString{String: text, Valid: true}
}

// storeLyrics keeps one song's lyrics in song_lyrics. A song without lyrics has
// no row, so a lyrics tag removed from the file is removed here too.
func storeLyrics(tx *sql.Tx, songID string, lyrics sql.NullString) error {
	if !lyrics.Valid {
		_, err := tx.Exec("DELETE FROM song_lyrics WHERE song_id = ?", songID)
		return err
	}
	_, err := tx.Exec(
		"INSERT INTO song_lyrics (song_id, text) VALUES (?, ?) "+
			"ON CONFLICT(song_id) DO UPDATE SET text = excluded.text",
		songID, lyrics.String)
	return err
}
