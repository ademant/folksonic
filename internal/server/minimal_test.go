package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPrefersMinimal(t *testing.T) {
	for h, want := range map[string]bool{
		"":                                 false,
		"tinysonic-minimal":                true,
		"TINYSONIC-MINIMAL":                true,
		"return=minimal":                   false,
		"respond-async, tinysonic-minimal": true,
		"tinysonic-minimal; foo=bar":       true,
		"tinysonic-minimalist":             false,
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		if h != "" {
			r.Header.Set("Prefer", h)
		}
		if got := prefersMinimal(r); got != want {
			t.Errorf("Prefer %q: %v, want %v", h, got, want)
		}
	}
}

var minimalHdr = map[string]string{"Prefer": "tinysonic-minimal"}

func TestMinimalResponses(t *testing.T) {
	s := pagingFixture(t)
	std := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-1"}}, nil)
	min := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-1"}}, minimalHdr)

	if std.header.Get("Preference-Applied") != "" || !strings.Contains(std.header.Get("Vary"), "Prefer") {
		t.Fatalf("standard headers: %v", std.header)
	}
	if min.header.Get("Preference-Applied") != "tinysonic-minimal" || !strings.Contains(min.header.Get("Vary"), "Prefer") {
		t.Fatalf("minimal headers: %v", min.header)
	}
	if len(min.body) >= len(std.body) || strings.Contains(min.body, "null") {
		t.Fatalf("minimal %d bytes vs standard %d: %s", len(min.body), len(std.body), min.body)
	}

	album := min.data["album"].(map[string]any)
	song := album["song"].([]any)[0].(map[string]any)
	for _, f := range []string{"title", "coverArt"} {
		if _, ok := album[f]; ok {
			t.Errorf("album still has %s", f)
		}
	}
	for _, f := range []string{"id", "name", "artistId", "songCount", "duration", "created", "mediaType", "musicBrainzId"} {
		if _, ok := album[f]; !ok {
			t.Errorf("album lost %s", f)
		}
	}
	for _, f := range minimalDrop["song"] {
		if _, ok := song[f]; ok {
			t.Errorf("song still has %s", f)
		}
	}
	for _, f := range []string{"id", "isDir", "title", "albumId", "artistId", "duration", "mediaType", "musicBrainzId", "genre", "track"} {
		if _, ok := song[f]; !ok {
			t.Errorf("song lost %s", f)
		}
	}

	// Artists in the index lose coverArt, keep albumCount.
	a := call(t, s, "GET", "getArtists", url.Values{}, minimalHdr).data["artists"].(map[string]any)["index"].([]any)[0].(map[string]any)["artist"].([]any)[0].(map[string]any)
	if _, ok := a["coverArt"]; ok {
		t.Error("artist still has coverArt")
	}
	if _, ok := a["albumCount"]; !ok {
		t.Error("artist lost albumCount")
	}
}

// Folder browsing keeps parent; minimal mode combines with paging; unknown
// preferences change nothing.
func TestMinimalFolderPagingAndOtherPreferences(t *testing.T) {
	s := pagingFixture(t)
	dir := call(t, s, "GET", "getMusicDirectory", url.Values{"id": {"al-3"}, "size": {"1"}, "offset": {"1"}}, minimalHdr)
	children := dir.data["directory"].(map[string]any)["child"].([]any)
	if len(children) != 1 {
		t.Fatalf("paged children: %v", children)
	}
	c := children[0].(map[string]any)
	if c["parent"] != "al-3" || c["id"] != "so-5" {
		t.Fatalf("folder child: %v", c)
	}
	if _, ok := c["path"]; ok {
		t.Fatal("folder child still has path")
	}

	std := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-1"}}, nil)
	other := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-1"}}, map[string]string{"Prefer": "return=minimal, foo"})
	if other.header.Get("Preference-Applied") != "" || other.body != std.body {
		t.Fatalf("other preferences changed the response: %v", other.header)
	}
}

// Streams are not wrapped: no Vary: Prefer and no Preference-Applied.
func TestMinimalNotAppliedToStreams(t *testing.T) {
	s := pagingFixture(t)
	req, _ := http.NewRequest("GET", "/rest/stream?id=so-1&u=admin&p=s3cret", nil)
	req.Header.Set("Prefer", "tinysonic-minimal")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Header().Get("Preference-Applied") != "" {
		t.Fatal("stream marked minimal")
	}
}
