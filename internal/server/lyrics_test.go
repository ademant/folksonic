package server

import (
	"strings"
	"testing"
)

// lyricsFixture stores lyrics for two songs of the paging fixture: so-4 is the
// only "Dance" by Charlie, and so-1 and so-2 are both "Same" by Alpha.
func lyricsFixture(t *testing.T) *State {
	t.Helper()
	s, _, _ := keyFixture(t)
	for _, q := range []string{
		"INSERT INTO song_lyrics (song_id, text) VALUES ('so-4', 'First line\r\n\r\nSecond line')",
		"INSERT INTO song_lyrics (song_id, text) VALUES ('so-1', 'Ambiguous')",
		"INSERT INTO song_lyrics (song_id, text) VALUES ('so-2', 'Ambiguous too')",
	} {
		if _, err := s.Pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// #142: getLyricsBySongId returns the stored text as one unsynchronised
// structuredLyrics entry with one line per row; a song without lyrics gets an
// empty list.
func TestGetLyricsBySongId(t *testing.T) {
	s := lyricsFixture(t)
	key := newKeyFor(t, s, "bob")
	body := getWith(t, s.routes(), "/rest/getLyricsBySongId.view?id=so-4", key, true).Body.String()
	list := subsonicBody(t, body)["lyricsList"].(map[string]any)["structuredLyrics"].([]any)
	if len(list) != 1 {
		t.Fatalf("structuredLyrics = %d entries, want 1: %s", len(list), body)
	}
	entry := list[0].(map[string]any)
	if entry["synced"] != false || entry["lang"] != "und" || entry["displayTitle"] != "Dance" {
		t.Fatalf("entry = %v", entry)
	}
	var values []string
	for _, l := range entry["line"].([]any) {
		values = append(values, l.(map[string]any)["value"].(string))
	}
	if strings.Join(values, "|") != "First line||Second line" {
		t.Fatalf("lines = %q", values)
	}

	body = getWith(t, s.routes(), "/rest/getLyricsBySongId.view?id=so-5", key, true).Body.String()
	if list := subsonicBody(t, body)["lyricsList"].(map[string]any)["structuredLyrics"].([]any); len(list) != 0 {
		t.Fatalf("song without lyrics: %s", body)
	}
}

func TestGetLyricsBySongIdErrors(t *testing.T) {
	s := lyricsFixture(t)
	key := newKeyFor(t, s, "bob")
	for path, want := range map[string]int{
		"/rest/getLyricsBySongId.view":            10,
		"/rest/getLyricsBySongId.view?id=so-nope": 70,
	} {
		if code := errorCode(t, getWith(t, s.routes(), path, key, true).Body.String()); code != want {
			t.Errorf("%s: code %d, want %d", path, code, want)
		}
	}
}

// getLyrics finds the one song by artist and title, ignoring case.
func TestGetLyricsByArtistAndTitle(t *testing.T) {
	s := lyricsFixture(t)
	key := newKeyFor(t, s, "bob")
	body := getWith(t, s.routes(), "/rest/getLyrics.view?artist=charlie&title=DANCE", key, true).Body.String()
	lyrics := subsonicBody(t, body)["lyrics"].(map[string]any)
	if lyrics["value"] != "First line\r\n\r\nSecond line" {
		t.Fatalf("value = %q: %s", lyrics["value"], body)
	}

	// Two songs with the name: no guess.
	body = getWith(t, s.routes(), "/rest/getLyrics.view?artist=Alpha&title=Same", key, true).Body.String()
	if _, ok := subsonicBody(t, body)["lyrics"].(map[string]any)["value"]; ok {
		t.Fatalf("ambiguous match returned a value: %s", body)
	}
}

// The songLyrics extension is advertised, because clients look for it before
// they call getLyricsBySongId.
func TestSongLyricsExtensionAdvertised(t *testing.T) {
	s := lyricsFixture(t)
	body := getWith(t, s.routes(), "/rest/getOpenSubsonicExtensions.view", "", true).Body.String()
	if !strings.Contains(body, `"name":"songLyrics"`) {
		t.Fatalf("songLyrics not advertised: %s", body)
	}
}
