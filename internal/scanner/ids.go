package scanner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// idHash is the hex SHA-256 of s, cut to 32 characters (128 bits). IDs are
// identifiers, not secrets, so only collision resistance matters here.
func idHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:32]
}

func ArtistID(name string) string {
	return "ar-" + idHash(strings.ToLower(name))
}

func AlbumID(artist, album string, year int64) string {
	return "al-" + idHash(fmt.Sprintf("%s|%s|%d", strings.ToLower(artist), strings.ToLower(album), year))
}

func SongID(path string) string {
	return "so-" + idHash(path)
}
