package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// keyFixture is the pagingFixture with the users "worker" and "bob". It
// returns a key with a scope, as the analysis worker keys removed in #105 had,
// and a full key for bob.
func keyFixture(t *testing.T) (*State, string, string) {
	t.Helper()
	s := pagingFixture(t)
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "worker", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	scoped, err := users.CreateScopedAPIKey(ctx, s.Pool, userIDOf(t, s, "worker"), "worker: desk", "worker", 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := users.CreateAPIKey(ctx, s.Pool, userIDOf(t, s, "bob"), "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, scoped, full
}

func userIDOf(t *testing.T, s *State, name string) int64 {
	t.Helper()
	u, err := users.ByName(context.Background(), s.Pool, name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// keyCall sends a request presenting key: in the X-Api-Key header, or in the
// apiKey query parameter when inHeader is false.
func keyCall(t *testing.T, s *State, key string, inHeader bool, method, path, body string) (int, map[string]any) {
	t.Helper()
	if key != "" && !inHeader {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "apiKey=" + key
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" && inHeader {
		req.Header.Set("X-Api-Key", key)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A key with a scope is refused everywhere, in either form, and the worker
// endpoints are gone (#105).
func TestScopedKeysAndWorkerEndpointsAreGone(t *testing.T) {
	s, scoped, full := keyFixture(t)
	for _, inHeader := range []bool{true, false} {
		if code, _ := keyCall(t, s, scoped, inHeader, "GET", "/api/library/stats", ""); code != http.StatusUnauthorized {
			t.Errorf("scoped key (header %v) on the library API: %d", inHeader, code)
		}
		code, body := keyCall(t, s, scoped, inHeader, "GET", "/rest/ping?f=json", "")
		if sr, _ := body["subsonic-response"].(map[string]any); code == http.StatusOK && sr["status"] == "ok" {
			t.Errorf("scoped key (header %v) opened a Subsonic endpoint: %v", inHeader, body)
		}
	}
	for _, p := range []string{"/api/worker/jobs", "/api/worker/results"} {
		if code, _ := keyCall(t, s, full, true, "POST", p, `{"tool":"folksentia"}`); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}
}
