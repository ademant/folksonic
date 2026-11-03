package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/models"
)

const mbid = "9a1f0c2e-5b7d-4e8a-9c3f-1d2e3f4a5b6c"

func TestExtensionsAdvertisedWithoutSignIn(t *testing.T) {
	s := newTestState(t)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getOpenSubsonicExtensions", nil))
	var out struct {
		SubsonicResponse struct {
			Status       string `json:"status"`
			OpenSubsonic bool   `json:"openSubsonic"`
			Extensions   []struct {
				Name     string `json:"name"`
				Versions []int  `json:"versions"`
			} `json:"openSubsonicExtensions"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	r := out.SubsonicResponse
	if r.Status != "ok" || !r.OpenSubsonic {
		t.Fatalf("envelope: %s", rec.Body.String())
	}
	names := map[string]bool{}
	for _, e := range r.Extensions {
		names[e.Name] = true
	}
	if !names["apiKeyAuthentication"] || !names["formPost"] {
		t.Fatalf("extensions not advertised: %s", rec.Body.String())
	}
}

func TestMusicBrainzIDInResponses(t *testing.T) {
	song := (&State{}).songToChild(models.Song{ID: "so-1", MBID: sql.NullString{String: mbid, Valid: true}})
	if song["musicBrainzId"] != mbid || song["mediaType"] != "song" {
		t.Fatalf("song: %v", song)
	}
	album := albumToChild(models.Album{ID: "al-1", MBID: sql.NullString{String: mbid, Valid: true}})
	if album["musicBrainzId"] != mbid || album["mediaType"] != "album" {
		t.Fatalf("album: %v", album)
	}
	artist := artistToJSON(models.Artist{ID: "ar-1"}, 0)
	if artist["musicBrainzId"] != "" || artist["mediaType"] != "artist" {
		t.Fatalf("artist without id must be empty: %v", artist)
	}
}

func TestSongTagsExposedOnlyWhenConfigured(t *testing.T) {
	song := (&State{}).songToChild(models.Song{ID: "so-1", Tags: sql.NullString{String: `{"DANCE":["Waltz","Mazurka"]}`, Valid: true}})
	tags, ok := song["tinysonicTags"].(map[string][]string)
	if !ok || !reflect.DeepEqual(tags["DANCE"], []string{"Waltz", "Mazurka"}) {
		t.Fatalf("tinysonicTags: %v", song["tinysonicTags"])
	}
	if _, ok := (&State{}).songToChild(models.Song{ID: "so-2"})["tinysonicTags"]; ok {
		t.Fatal("tinysonicTags present without tags")
	}

	s := newTestState(t)
	s.SongTags = []string{"DANCE"}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getOpenSubsonicExtensions", nil))
	if !strings.Contains(rec.Body.String(), `"name":"tinysonicSongTags"`) {
		t.Fatalf("vendor extension not advertised when song_tags is set: %s", rec.Body.String())
	}
	s.SongTags = nil
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getOpenSubsonicExtensions", nil))
	if strings.Contains(rec.Body.String(), "tinysonicSongTags") {
		t.Fatalf("vendor extension advertised without song_tags: %s", rec.Body.String())
	}
}
