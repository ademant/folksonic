package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

func TestPasskeysUnavailableWithoutBaseURL(t *testing.T) {
	s := newTestState(t) // webAuthn is nil: base_url not set
	req := httptest.NewRequest(http.MethodPost, "/api/passkey/login/begin", strings.NewReader(`{"username":"admin"}`))
	rec := httptest.NewRecorder()
	s.APIPasskeyLoginBegin(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 without base_url", rec.Code)
	}
}

func TestPasswordLoginIssuesSession(t *testing.T) {
	s := newTestState(t)
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"s3cret"}`))
	rec := httptest.NewRecorder()
	s.APILogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no token: %s", rec.Body.String())
	}
	// The session token authenticates as a Bearer on API calls.
	req = httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec = httptest.NewRecorder()
	s.APIListKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer auth status %d", rec.Code)
	}
}

func TestPasswordLoginRejectsWrongPasswordAndPending(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "dave", "pw", users.RoleUser, users.StatusPending); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"username":"admin","password":"wrong"}`,
		`{"username":"dave","password":"pw"}`,
		`{"username":"nobody","password":"x"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.APILogin(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", body, rec.Code)
		}
	}
}

func TestLoginPageServed(t *testing.T) {
	s := newTestState(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.ServeLoginPage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sign in to tinysonic") {
		t.Fatalf("login page not served: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("login page missing CSP header")
	}
}

// An expired API key must stop authenticating; a key with no expiry keeps working.
func TestAPIKeyExpiry(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	// A key whose expiry is an hour in the past.
	expired, err := users.CreateAPIKey(ctx, s.Pool, admin.ID, "old", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	if _, err := s.Pool.Exec("UPDATE api_keys SET expires_at = ? WHERE label = 'old'", past); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+expired, nil)); ok {
		t.Fatal("expired API key still authenticates")
	}
	live, _ := users.CreateAPIKey(ctx, s.Pool, admin.ID, "new", time.Hour)
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+live, nil)); !ok {
		t.Fatal("unexpired API key rejected")
	}
}
