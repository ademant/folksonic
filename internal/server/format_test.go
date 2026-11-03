package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/models"
)

func TestRelativePath(t *testing.T) {
	a, b := filepath.FromSlash("/srv/music"), filepath.FromSlash("/data/more")
	for _, tc := range []struct {
		dirs []string
		path string
		want string
	}{
		{[]string{a}, "/srv/music/War-Sav/Ar_Vuhez/01.opus", "War-Sav/Ar_Vuhez/01.opus"},
		{[]string{a, b}, "/data/more/x/y.flac", "x/y.flac"},
		{[]string{a, filepath.Join(a, "War-Sav")}, "/srv/music/War-Sav/01.opus", "01.opus"},
		{[]string{a}, "/srv/musicbox/01.opus", "01.opus"}, // sibling with common prefix, not inside
		{[]string{a}, "/elsewhere/z.mp3", "z.mp3"},
	} {
		got := relativePath(tc.dirs, filepath.FromSlash(tc.path))
		if got != tc.want || strings.HasPrefix(got, "/") {
			t.Errorf("relativePath(%v, %q) = %q, want %q", tc.dirs, tc.path, got, tc.want)
		}
	}
}

func TestStripArticle(t *testing.T) {
	def := strings.Fields(DefaultIgnoredArticles)
	for in, want := range map[string]string{
		"The Beatles":       "Beatles",
		"Les Frères Morvan": "Frères Morvan",
		"La Bottine":        "Bottine",
		"Le Bour-Bodros":    "Bour-Bodros",
		"El Niño":           "Niño",
		"An Erminig":        "Erminig",
		"Die Streuner":      "Streuner",
		"L'Ham":             "Ham",
		"l’Héritage":        "Héritage",
		"L' Espace":         "Espace",
		"The":               "The",
		"L'":                "L'",
		"Theodor":           "Theodor",
		"Lena":              "Lena",
		"Annie":             "Annie",
		"Tri Yann":          "Tri Yann",
	} {
		if got := stripArticle(in, def); got != want {
			t.Errorf("stripArticle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := stripArticle("The Beatles", []string{"Der"}); got != "The Beatles" {
		t.Errorf("custom list: %q", got)
	}
	if got := stripArticle("Der Plan", []string{"Der"}); got != "Plan" {
		t.Errorf("custom list: %q", got)
	}
	if got := stripArticle("The Beatles", []string{}); got != "The Beatles" {
		t.Errorf("empty list: %q", got)
	}
}

func TestArtistIndexSkipsArticles(t *testing.T) {
	artists := []models.Artist{
		{ID: "ar-1", Name: "Les Frères Morvan"},
		{ID: "ar-2", Name: "Fest Noz"},
		{ID: "ar-3", Name: "An Erminig"},
		{ID: "ar-4", Name: "The"},
		{ID: "ar-5", Name: "42"},
	}
	index := buildArtistIndex(artists, nil, strings.Fields(DefaultIgnoredArticles))
	got := map[string][]string{}
	var order []string
	for _, g := range index {
		letter := g["name"].(string)
		order = append(order, letter)
		for _, a := range g["artist"].([]J) {
			got[letter] = append(got[letter], a["name"].(string))
		}
	}
	if strings.Join(order, ",") != "#,E,F,T" {
		t.Fatalf("groups = %v", order)
	}
	if strings.Join(got["F"], "|") != "Fest Noz|Les Frères Morvan" {
		t.Fatalf("F = %v (sorted without article, names unchanged)", got["F"])
	}
	if strings.Join(got["E"], "|") != "An Erminig" || strings.Join(got["T"], "|") != "The" {
		t.Fatalf("E = %v, T = %v", got["E"], got["T"])
	}
}

func TestPruneNulls(t *testing.T) {
	body := J{"a": nil, "b": 1, "c": J{"d": nil, "e": []J{{"f": nil, "g": "x"}}}, "h": []any{J{"i": nil}}}
	pruneNulls(body)
	out, _ := json.Marshal(body)
	if strings.Contains(string(out), "null") || string(out) != `{"b":1,"c":{"e":[{"g":"x"}]},"h":[{}]}` {
		t.Fatalf("pruned = %s", out)
	}
}

// Library responses: no nulls, relative paths, real created dates, one album
// duration everywhere, newest = recently added, and the announced articles.
func TestLibraryResponses(t *testing.T) {
	s := newTestState(t)
	s.MusicDirs = []string{"/srv/music"}
	for _, q := range []string{
		`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'Les Ogres', 'les ogres')`,
		`INSERT INTO albums (id, title, artist, artist_id, year) VALUES
			('al-old', 'Old', 'Les Ogres', 'ar-1', 2024),
			('al-new', 'New', 'Les Ogres', 'ar-1', 0)`,
		// 1999 ms + 1999 ms: truncating per song gives 2 s, summing first gives 3 s.
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, duration_ms, created_at) VALUES
			('so-1', '/srv/music/Ogres/Old/1.mp3', 'One', 'Les Ogres', 'ar-1', 'Old', 'al-old', 'mp3', 'audio/mpeg', 1999, '2024-05-01T10:00:00Z'),
			('so-2', '/srv/music/Ogres/Old/2.mp3', 'Two', 'Les Ogres', 'ar-1', 'Old', 'al-old', 'mp3', 'audio/mpeg', 1999, '2024-05-02T10:00:00Z'),
			('so-3', '/srv/music/Ogres/New/1.mp3', 'Three', 'Les Ogres', 'ar-1', 'New', 'al-new', 'mp3', 'audio/mpeg', 1000, '2026-11-01T10:00:00Z')`,
	} {
		if _, err := s.Pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"&u=admin&p=s3cret&f=json", nil))
		if strings.Contains(rec.Body.String(), "null") {
			t.Errorf("%s contains null: %s", path, rec.Body.String())
		}
		var env map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		r := env["subsonic-response"]
		if r["status"] != "ok" {
			t.Fatalf("%s: %s", path, rec.Body.String())
		}
		return r
	}

	album := get("/rest/getAlbum?id=al-old")["album"].(map[string]any)
	if album["duration"] != 3.0 || album["created"] != "2024-05-01T10:00:00Z" || album["songCount"] != 2.0 {
		t.Fatalf("getAlbum: duration %v, created %v, songCount %v", album["duration"], album["created"], album["songCount"])
	}
	song := album["song"].([]any)[0].(map[string]any)
	if song["path"] != "Ogres/Old/1.mp3" || song["created"] != "2024-05-01T10:00:00Z" {
		t.Fatalf("song path %v, created %v", song["path"], song["created"])
	}
	if _, ok := song["genre"]; ok {
		t.Fatal("song without genre carries a genre field")
	}

	list := get("/rest/getAlbumList2?type=newest")["albumList2"].(map[string]any)["album"].([]any)
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if first["id"] != "al-new" || second["id"] != "al-old" {
		t.Fatalf("newest order: %v, %v (want recently added first, regardless of year)", first["id"], second["id"])
	}
	if second["duration"] != album["duration"] || second["created"] != album["created"] {
		t.Fatalf("list and getAlbum differ: %v/%v vs %v/%v", second["duration"], second["created"], album["duration"], album["created"])
	}
	if _, ok := first["year"]; ok {
		t.Fatal("album without year carries a year field")
	}

	artists := get("/rest/getArtists?x=1")["artists"].(map[string]any)
	if artists["ignoredArticles"] != DefaultIgnoredArticles {
		t.Fatalf("ignoredArticles = %v", artists["ignoredArticles"])
	}
	if idx := artists["index"].([]any)[0].(map[string]any); idx["name"] != "O" {
		t.Fatalf("Les Ogres indexed under %v, want O", idx["name"])
	}

	// search3 with an empty query and artists requested used to fail on a
	// column mismatch.
	res := get("/rest/search3?query=&artistCount=10&albumCount=0&songCount=0")["searchResult3"].(map[string]any)
	if a := res["artist"].([]any); len(a) != 1 {
		t.Fatalf("empty-query artist search: %v", res)
	}
}
