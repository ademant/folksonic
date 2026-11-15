package server

import (
	"net/url"
	"strings"
	"testing"
)

// Songs carry tinysonicTempo with the known keys only, in full and minimal
// responses, and the extension is advertised (#127).
func TestSongTempoField(t *testing.T) {
	s := pagingFixture(t)
	if _, err := s.Pool.Exec(`UPDATE songs SET bpm = 124, bpm_source = 'folksentia', bpm_confidence = 0.92 WHERE id = 'so-4';
		UPDATE songs SET bpm = 90, bpm_source = 'tag' WHERE id = 'so-5';
		INSERT INTO song_values (song_id, name, value, changed_ms) VALUES
			('so-4', 'rhythm', '{"pulseSec":0.527,"barPulses":3,"barsPerMin":38,"barScore":0.34}', 1),
			('so-3', 'rhythm', '{"pulseSec":null,"barPulses":null,"barsPerMin":null,"barScore":null}', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []map[string]string{nil, minimalHdr} {
		songs := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-3"}}, hdr).data["album"].(map[string]any)["song"].([]any)
		a, b := songs[0].(map[string]any), songs[1].(map[string]any)
		tempo, ok := a["tinysonicTempo"].(map[string]any)
		if !ok || len(tempo) != 3 || tempo["confidence"] != 0.92 || tempo["barsPerMin"] != 38.0 || tempo["barPulses"] != 3.0 {
			t.Fatalf("tinysonicTempo: %v", a["tinysonicTempo"])
		}
		// A tag BPM has no confidence and no rhythm: no object.
		if _, ok := b["tinysonicTempo"]; ok || b["bpm"] != 90.0 {
			t.Fatalf("tag-only song: %v", b)
		}
	}
	// A rhythm with every value unknown gives no object either.
	song := call(t, s, "GET", "getSong", url.Values{"id": {"so-3"}}, nil).data["song"].(map[string]any)
	if _, ok := song["tinysonicTempo"]; ok {
		t.Fatalf("unknown rhythm: %v", song["tinysonicTempo"])
	}
	ext := call(t, s, "GET", "getOpenSubsonicExtensions", url.Values{}, nil).body
	if !strings.Contains(ext, `{"name":"tinysonicTempo","versions":[1]}`) {
		t.Fatalf("tinysonicTempo not advertised: %s", ext)
	}
}
