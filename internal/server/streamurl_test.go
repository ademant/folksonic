package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// streamFixture gives song so-1 a real file to serve and returns bob's session
// token and user id.
func streamFixture(t *testing.T) (*State, string, int64) {
	t.Helper()
	s, _, _ := keyFixture(t)
	audio := filepath.Join(t.TempDir(), "track.mp3")
	if err := os.WriteFile(audio, []byte("audio bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE songs SET path = ? WHERE id = 'so-1'", audio); err != nil {
		t.Fatal(err)
	}
	bob := userIDOf(t, s, "bob")
	tok, err := users.CreateSession(context.Background(), s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	return s, tok, bob
}

// mintStreamURL asks /api/stream-url as the session and returns the URL.
func mintStreamURL(t *testing.T, s *State, token, songID string) *url.URL {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/stream-url?id="+songID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// getURL requests a parsed URL with no other credentials.
func getURL(t *testing.T, s *State, u *url.URL) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	return rec
}

// withQuery returns a copy of u with one query parameter replaced.
func withQuery(u *url.URL, key, value string) *url.URL {
	c := *u
	q := c.Query()
	q.Set(key, value)
	c.RawQuery = q.Encode()
	return &c
}

func TestSignedStreamURLPlaysItsSong(t *testing.T) {
	s, tok, _ := streamFixture(t)
	rec := getURL(t, s, mintStreamURL(t, s, tok, "so-1"))
	if rec.Code != http.StatusOK || rec.Body.String() != "audio bytes" {
		t.Fatalf("signed URL: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestSignedStreamURLRefusesChanges(t *testing.T) {
	s, tok, bob := streamFixture(t)
	good := mintStreamURL(t, s, tok, "so-1")
	exp, _ := strconv.ParseInt(good.Query().Get("exp"), 10, 64)

	for name, u := range map[string]*url.URL{
		"other song":   withQuery(good, "id", "so-2"),
		"later expiry": withQuery(good, "exp", strconv.FormatInt(exp+60, 10)),
		"other user":   withQuery(good, "uid", strconv.FormatInt(bob+1, 10)),
		"forged sig":   withQuery(good, "sig", strings.Repeat("0", 64)),
		"junk sig":     withQuery(good, "sig", "not-hex"),
	} {
		rec := getURL(t, s, u)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
}

func TestSignedStreamURLExpires(t *testing.T) {
	s, _, bob := streamFixture(t)
	secret, err := s.streamSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).Unix()
	q := url.Values{
		"id":  {"so-1"},
		"uid": {strconv.FormatInt(bob, 10)},
		"exp": {strconv.FormatInt(past, 10)},
		"sig": {users.SignStream(secret, "so-1", bob, past)},
	}
	rec := getURL(t, s, &url.URL{Path: "/rest/stream.view", RawQuery: q.Encode()})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired link: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestRotatingTheKeyInvalidatesLinksAfterRestart(t *testing.T) {
	s, tok, _ := streamFixture(t)
	u := mintStreamURL(t, s, tok, "so-1")
	if _, err := users.RotateStreamSecret(context.Background(), s.Pool); err != nil {
		t.Fatal(err)
	}
	// Still the old key in memory: the link keeps working until a restart.
	if rec := getURL(t, s, u); rec.Code != http.StatusOK {
		t.Fatalf("before restart: status %d", rec.Code)
	}
	s.forgetStreamSecret() // what a restart does
	if rec := getURL(t, s, u); rec.Code != http.StatusForbidden {
		t.Fatalf("after rotation: status %d, want 403", rec.Code)
	}
}

func TestSignedURLOnlyWorksOnStreams(t *testing.T) {
	s, tok, _ := streamFixture(t)
	u := mintStreamURL(t, s, tok, "so-1")
	u.Path = "/rest/getPlaylists.view"
	if rec := getURL(t, s, u); rec.Code != http.StatusForbidden {
		t.Fatalf("signed URL on ping: status %d, want 403", rec.Code)
	}
}

func TestAPIKeyCannotMintStreamURLs(t *testing.T) {
	s, _, key := keyFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/stream-url?id=so-1", nil)
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), users.CodeSessionRequired) {
		t.Fatalf("key minted a link: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestStreamURLExtensionAdvertised(t *testing.T) {
	s, _, key := keyFixture(t)
	rec := getWith(t, s.routes(), "/rest/getOpenSubsonicExtensions.view", key, true)
	if !strings.Contains(rec.Body.String(), "tinysonicStreamUrl") {
		t.Fatalf("extension not advertised: %s", rec.Body.String())
	}
}
