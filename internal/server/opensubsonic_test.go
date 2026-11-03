package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	song := songToChild(models.Song{ID: "so-1", MBID: sql.NullString{String: mbid, Valid: true}})
	if song["musicBrainzId"] != mbid || song["mediaType"] != "song" {
		t.Fatalf("song: %v", song)
	}
	album := albumToChild(models.Album{ID: "al-1", MBID: sql.NullString{String: mbid, Valid: true}}, 1, 1)
	if album["musicBrainzId"] != mbid || album["mediaType"] != "album" {
		t.Fatalf("album: %v", album)
	}
	artist := artistToJSON(models.Artist{ID: "ar-1"}, 0)
	if artist["musicBrainzId"] != "" || artist["mediaType"] != "artist" {
		t.Fatalf("artist without id must be empty: %v", artist)
	}
}
