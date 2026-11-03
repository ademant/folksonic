package scanner

import (
	"database/sql"
	"testing"

	"github.com/dhowden/tag"
)

const trackUUID = "9a1f0c2e-5b7d-4e8a-9c3f-1d2e3f4a5b6c"

func TestReadMBIDPerFormat(t *testing.T) {
	cases := map[string]map[string]any{
		"vorbis (FLAC, Opus)": {"musicbrainz_trackid": "9A1F0C2E-5B7D-4E8A-9C3F-1D2E3F4A5B6C"},
		"id3 TXXX frame":      {"TXXX": &tag.Comm{Description: "MusicBrainz Track Id", Text: trackUUID}},
		"mp4 freeform atom":   {"MusicBrainz Track Id": []string{trackUUID}},
	}
	for name, raw := range cases {
		got := readMBID(raw, trackMBID)
		if !got.Valid || got.String != trackUUID {
			t.Errorf("%s: got %+v, want %s", name, got, trackUUID)
		}
	}
}

func TestReadMBIDRejectsMalformedAndMissing(t *testing.T) {
	if got := readMBID(map[string]any{"musicbrainz_trackid": "not-a-uuid"}, trackMBID); got != (sql.NullString{}) {
		t.Errorf("malformed id kept: %+v", got)
	}
	if got := readMBID(map[string]any{"title": "x"}, trackMBID); got != (sql.NullString{}) {
		t.Errorf("missing id reported: %+v", got)
	}
}

func TestReadMBIDKindsAreSeparate(t *testing.T) {
	raw := map[string]any{"musicbrainz_albumid": "1b2c3d4e-5f60-4718-8293-a4b5c6d7e8f9"}
	if got := readMBID(raw, trackMBID); got.Valid {
		t.Errorf("album id read as track id: %+v", got)
	}
	if got := readMBID(raw, albumMBID); !got.Valid {
		t.Errorf("album id not read")
	}
}
