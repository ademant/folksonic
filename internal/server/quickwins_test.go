package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// fullChain is the middleware that wraps every response in production, minus
// the request logger and body limit.
func fullChain(s *State) http.Handler {
	return securityHeaders(cacheDefaults(s.routes()))
}

func getWith(t *testing.T, h http.Handler, path string, key string, inHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	if key != "" && !inHeader {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "apiKey=" + key
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" && inHeader {
		req.Header.Set("X-Api-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// #88: personal responses are not stored; audio and covers are private.
func TestCacheControlOnSubsonicAndAPIResponses(t *testing.T) {
	s, _, key := keyFixture(t)
	h := fullChain(s)

	for _, c := range []struct{ path, want string }{
		{"/rest/ping.view", "no-store"},
		{"/api/keys", "no-store"},
		{"/rest/stream.view?id=so-1", "private, max-age=3600"},
		{"/rest/download.view?id=so-1", "private, max-age=3600"},
		{"/rest/getCoverArt.view?id=so-1", "private, max-age=86400"},
	} {
		rec := getWith(t, h, c.path, key, false)
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.want)
		}
	}
}

func TestCacheControlForPaths(t *testing.T) {
	for p, want := range map[string]string{
		"/rest/stream":            "private, max-age=3600",
		"/rest/stream.view":       "private, max-age=3600",
		"/rest/download.view":     "private, max-age=3600",
		"/rest/getCoverArt.view":  "private, max-age=86400",
		"/rest/getPlaylists.view": "no-store",
		"/api/analysis":           "no-store",
		"/rest/streamer.view":     "no-store",
	} {
		if got := cacheControlFor(p); got != want {
			t.Errorf("cacheControlFor(%q) = %q, want %q", p, got, want)
		}
	}
}

// #90: the referrer is not sent and sensor features are off, on every response.
func TestSecurityHeadersOnPageApiAndStream(t *testing.T) {
	s, _, key := keyFixture(t)
	h := fullChain(s)
	for _, path := range []string{"/", "/api/keys", "/rest/stream.view?id=so-1"} {
		rec := getWith(t, h, path, key, true)
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy %q", path, got)
		}
		if got := rec.Header().Get("Permissions-Policy"); !strings.Contains(got, "camera=()") || !strings.Contains(got, "geolocation=()") {
			t.Errorf("%s: Permissions-Policy %q", path, got)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options lost", path)
		}
	}
}

// #84: a plain song id is logged on request lines; anything else is not.
func TestSongIDFieldLogsOnlyPlainTokens(t *testing.T) {
	for _, c := range []struct{ query, want string }{
		{"id=so-8a53ae3703a246595696e2b3a7c1fb04", " id=so-8a53ae3703a246595696e2b3a7c1fb04"},
		{"id=so-1&apiKey=tsk_secret", " id=so-1"},
		{"id=so 1", ""},
		{"id=so-1%0Aforged", ""},
		{"id=" + strings.Repeat("a", 65), ""},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/rest/stream", nil)
		r.URL.RawQuery = c.query
		if got := songIDField(r); got != c.want {
			t.Errorf("songIDField(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

// #89: each refused key or session gets its reason; the key itself never does.
func TestCredentialProblemReasons(t *testing.T) {
	s, _, _ := keyFixture(t)
	ctx := context.Background()
	bob := userIDOf(t, s, "bob")

	active, err := users.CreateAPIKey(ctx, s.Pool, bob, "active", 0)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := users.CreateAPIKey(ctx, s.Pool, bob, "revoked phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := users.CreateAPIKey(ctx, s.Pool, bob, "old tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := users.ListAPIKeys(ctx, s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	idOf := map[string]int64{}
	for _, k := range keys {
		idOf[k.Label] = k.ID
	}
	if _, err := s.Pool.Exec("UPDATE api_keys SET revoked_at = '2026-01-01T00:00:00Z' WHERE id = ?", idOf["revoked phone"]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE api_keys SET expires_at = '2020-01-01T00:00:00Z' WHERE id = ?", idOf["old tablet"]); err != nil {
		t.Fatal(err)
	}

	if _, _, err := users.LookupAPIKeyInfo(ctx, s.Pool, active); err != nil {
		t.Fatalf("active key refused: %v", err)
	}
	for _, c := range []struct {
		key, want string
		id        int64
	}{
		{"tsk_never-issued", users.CodeKeyUnknown, 0},
		{revoked, users.CodeKeyRevoked, idOf["revoked phone"]},
		{expired, users.CodeKeyExpired, idOf["old tablet"]},
	} {
		reason, id, _ := users.APIKeyProblem(ctx, s.Pool, c.key)
		if reason != c.want || id != c.id {
			t.Errorf("APIKeyProblem: got (%s, %d), want (%s, %d)", reason, id, c.want, c.id)
		}
	}

	if got := users.SessionProblem(ctx, s.Pool, "not-a-session"); got != users.CodeSessionUnknown {
		t.Errorf("SessionProblem unknown: %q", got)
	}
	tok, err := users.CreateSession(ctx, s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE sessions SET expires_at = '2020-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	if got := users.SessionProblem(ctx, s.Pool, tok); got != users.CodeSessionExpired {
		t.Errorf("SessionProblem expired: %q", got)
	}
}

func TestRefusedKeyIsLoggedWithReasonAndNoSecret(t *testing.T) {
	s, _, _ := keyFixture(t)
	ctx := context.Background()
	bob := userIDOf(t, s, "bob")
	key, err := users.CreateAPIKey(ctx, s.Pool, bob, "old phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.RevokeAPIKey(ctx, s.Pool, bob, mustKeyID(t, s, bob, "old phone")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(&buf, slog.LevelDebug, false)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := getWith(t, fullChain(s), "/api/keys", key, true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN auth failed") || !strings.Contains(out, "reason=key_revoked") {
		t.Errorf("log line missing key_revoked warning: %q", out)
	}
	if !strings.Contains(out, `label="old phone"`) {
		t.Errorf("log line lacks the key label: %q", out)
	}
	if strings.Contains(out, key) {
		t.Errorf("log line contains the key itself: %q", out)
	}
}

func mustKeyID(t *testing.T, s *State, userID int64, label string) int64 {
	t.Helper()
	keys, err := users.ListAllAPIKeys(context.Background(), s.Pool, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Label == label {
			return k.ID
		}
	}
	t.Fatalf("no key %q for user %d", label, userID)
	return 0
}
