package scanner

import (
	"database/sql"
	"regexp"
	"strings"

	"github.com/dhowden/tag"
)

// mbidKind names one of the three MusicBrainz IDs a file can carry.
type mbidKind int

const (
	trackMBID  mbidKind = iota // recording
	albumMBID                  // release
	artistMBID                 // artist
)

// mbidNames gives the name each format uses for an ID, as MusicBrainz Picard
// and similar tools write it. Vorbis comments (FLAC, Ogg/Opus) use an upper-case
// key; ID3 (MP3) and MP4 use a user-defined text field or freeform atom with the
// description below.
var mbidNames = map[mbidKind]struct{ vorbis, description string }{
	trackMBID:  {"MUSICBRAINZ_TRACKID", "MusicBrainz Track Id"},
	albumMBID:  {"MUSICBRAINZ_ALBUMID", "MusicBrainz Album Id"},
	artistMBID: {"MUSICBRAINZ_ARTISTID", "MusicBrainz Artist Id"},
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// readMBID returns the MusicBrainz ID of the given kind from a file's raw tags,
// lower-cased, or an invalid NullString when the tag is missing or is not a
// UUID. Only well-formed UUIDs are kept, because the value is returned to
// clients as is.
func readMBID(raw map[string]any, kind mbidKind) sql.NullString {
	names := mbidNames[kind]
	for k, v := range raw {
		var candidate string
		switch val := v.(type) {
		case string:
			if strings.EqualFold(k, names.vorbis) {
				candidate = val
			}
		case []string: // MP4 freeform atom
			if strings.EqualFold(k, names.description) && len(val) > 0 {
				candidate = val[0]
			}
		case *tag.Comm: // ID3 TXXX frame
			if strings.EqualFold(val.Description, names.description) {
				candidate = val.Text
			}
		}
		id := strings.ToLower(strings.TrimSpace(candidate))
		if uuidPattern.MatchString(id) {
			return sql.NullString{String: id, Valid: true}
		}
	}
	return sql.NullString{}
}
