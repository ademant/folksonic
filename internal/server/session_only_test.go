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

// sessionOnlyCalls are the account and library endpoints that take a session
// and refuse an API key (#86).
var sessionOnlyCalls = []struct{ method, path, body string }{
	{http.MethodPost, "/api/keys", `{"label":"x"}`},
	{http.MethodGet, "/api/keys", ""},
	{http.MethodDelete, "/api/keys/1", ""},
	{http.MethodPost, "/api/pair/start", ""},
	{http.MethodPost, "/api/pair/qr", ""},
	{http.MethodGet, "/api/passkeys", ""},
	{http.MethodDelete, "/api/passkeys/1", ""},
	{http.MethodGet, "/api/oidc/identities", ""},
	{http.MethodDelete, "/api/oidc/identities/1", ""},
	{http.MethodPost, "/api/library/scan", ""},
	{http.MethodPost, "/api/library/analysis", `{"all":true}`},
	{http.MethodGet, "/api/library/stats", ""},
}

// authCall sends a request through the full router, with the credential in the
// X-Api-Key header, as a bearer token, or not at all.
func authCall(t *testing.T, s *State, method, path, body, apiKey, bearer string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	return rec.Code, decodeJSONLoose(rec.Body.String())
}

func decodeJSONLoose(body string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	return m
}

func TestAPIKeyCannotManageTheAccount(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, c := range sessionOnlyCalls {
		code, body := authCall(t, s, c.method, c.path, c.body, key, "")
		if code != http.StatusForbidden || body["error"] != users.CodeSessionRequired {
			t.Errorf("%s %s with a key: %d %v, want 403 session_required", c.method, c.path, code, body)
		}
	}
}

func TestSessionStillManagesTheAccount(t *testing.T) {
	s, _, _ := keyFixture(t)
	bob := userIDOf(t, s, "bob")
	tok, err := users.CreateSession(context.Background(), s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := authCall(t, s, http.MethodGet, "/api/keys", "", "", tok); code != http.StatusOK {
		t.Errorf("GET /api/keys with a session: %d %v, want 200", code, body)
	}
	if code, body := authCall(t, s, http.MethodPost, "/api/keys", `{"label":"tablet"}`, "", tok); code != http.StatusCreated {
		t.Errorf("POST /api/keys with a session: %d %v, want 201", code, body)
	}
}

func TestAPIKeyStillWorksForSubsonicAndAnalysis(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, path := range []string{"/rest/ping.view", "/api/analysis?id=so-1", "/api/duplicates"} {
		// /api/duplicates answers 202 while its search runs in the background.
		if code, body := authCall(t, s, http.MethodGet, path, "", key, ""); code != http.StatusOK && code != http.StatusAccepted {
			t.Errorf("GET %s with a key: %d %v, want 200", path, code, body)
		}
	}
}

func TestSessionOnlyRefusalWithoutCredentialsIsUnchanged(t *testing.T) {
	s, _, _ := keyFixture(t)
	if code, body := authCall(t, s, http.MethodGet, "/api/keys", "", "", ""); code != http.StatusUnauthorized || body["error"] != users.CodeCredentialsRequired {
		t.Errorf("no credentials: %d %v, want 401 credentials_required", code, body)
	}
}
