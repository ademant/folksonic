package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
	"github.com/tsirysndr/tinysonic/internal/passwords"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// newTestState returns a State backed by a fresh database containing one
// active admin, "admin" with password "s3cret".
func newTestState(t *testing.T) *State {
	t.Helper()
	pool, err := db.Init(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err := users.Create(context.Background(), pool, "admin", "s3cret", users.RoleLibrarian, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	return &State{Pool: pool}
}

func postForm(t *testing.T, s *State, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Ping(rec, req)
	return rec
}

func TestAuthCheck(t *testing.T) {
	const user, pass = "admin", "s3cret"
	hash, err := passwords.Hash(pass)
	if err != nil {
		t.Fatal(err)
	}
	enc := hex.EncodeToString([]byte(pass))

	tests := []struct {
		name string
		u, p string
		want bool
	}{
		{"plaintext ok", user, pass, true},
		{"plaintext wrong", user, "nope", false},
		{"enc hex ok", user, "enc:" + enc, true},
		{"enc hex wrong", user, "enc:" + hex.EncodeToString([]byte("x")), false},
		{"wrong user", "mallory", pass, false},
		{"missing user", "", pass, false},
		{"no credentials", user, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := authCheck(user, hash, tc.u, tc.p); got != tc.want {
				t.Fatalf("authCheck = %v, want %v", got, tc.want)
			}
		})
	}
}

// Subsonic token auth (t with s) is not supported. A request that uses only t
// and s gets error 41, as OpenSubsonic specifies (#170), so the client can say
// so, rather than error 40 (wrong password).
func TestTokenAuthAnsweredWithError41(t *testing.T) {
	s := newTestState(t)
	sum := md5.Sum([]byte("s3cret" + "abc123"))
	token := hex.EncodeToString(sum[:])

	tokenOnly := httptest.NewRecorder()
	s.routes().ServeHTTP(tokenOnly, httptest.NewRequest(http.MethodGet,
		"/rest/ping?u=admin&t="+token+"&s=abc123&v=1.16.1&c=test&f=json", nil))
	if !strings.Contains(tokenOnly.Body.String(), `"code":41`) {
		t.Fatalf("token-only request: want error 41, got %s", tokenOnly.Body.String())
	}

	wrongPassword := httptest.NewRecorder()
	s.routes().ServeHTTP(wrongPassword, httptest.NewRequest(http.MethodGet,
		"/rest/ping?u=admin&p=nope&v=1.16.1&c=test&f=json", nil))
	if !strings.Contains(wrongPassword.Body.String(), `"code":40`) {
		t.Fatalf("wrong password: want error 40, got %s", wrongPassword.Body.String())
	}

	plain := httptest.NewRecorder()
	s.routes().ServeHTTP(plain, httptest.NewRequest(http.MethodGet,
		"/rest/ping?u=admin&p=s3cret&v=1.16.1&c=test&f=json", nil))
	if !strings.Contains(plain.Body.String(), `"status":"ok"`) {
		t.Fatalf("plain password: %s", plain.Body.String())
	}
}

// POST form bodies must authenticate, not only query strings.
func TestPingAuthenticatesPOSTForm(t *testing.T) {
	s := newTestState(t)
	rec := postForm(t, s, "/rest/ping", url.Values{"u": {"admin"}, "p": {"s3cret"}, "v": {"1.16.1"}, "c": {"test"}})
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("POST form auth failed: %s", rec.Body.String())
	}
}

func TestPingRejectsWrongPOSTPassword(t *testing.T) {
	s := newTestState(t)
	rec := postForm(t, s, "/rest/ping", url.Values{"u": {"admin"}, "p": {"wrong"}})
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("wrong password accepted: %s", rec.Body.String())
	}
}

func TestPendingUserCannotAuthenticate(t *testing.T) {
	s := newTestState(t)
	if _, err := users.Create(context.Background(), s.Pool, "newbie", "pw", users.RoleUser, users.StatusPending); err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, s, "/rest/ping", url.Values{"u": {"newbie"}, "p": {"pw"}})
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("pending user authenticated: %s", rec.Body.String())
	}
}

func TestAPIKeyAuthenticates(t *testing.T) {
	s := newTestState(t)
	admin, _ := users.ByName(context.Background(), s.Pool, "admin")
	key, err := users.CreateAPIKey(context.Background(), s.Pool, admin.ID, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, s, "/rest/ping", url.Values{"apiKey": {key}})
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("API key rejected: %s", rec.Body.String())
	}
	// A bad key must not fall through to other credentials.
	rec = postForm(t, s, "/rest/ping", url.Values{"apiKey": {"tsk_bogus"}, "u": {"admin"}, "p": {"s3cret"}})
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("invalid API key fell through to password auth")
	}
}

func TestRevokedAPIKeyRejected(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	key, _ := users.CreateAPIKey(ctx, s.Pool, admin.ID, "phone", 0)
	keys, _ := users.ListAPIKeys(ctx, s.Pool, admin.ID)
	if err := users.RevokeAPIKey(ctx, s.Pool, admin.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	rec := postForm(t, s, "/rest/ping", url.Values{"apiKey": {key}})
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatal("revoked API key still works")
	}
}

// An API key together with u, p, t or s is a conflict: OpenSubsonic error 43,
// in the query and in the header, even with a valid key (#170).
func TestAPIKeyWithOtherCredentialsIsError43(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, extra := range []string{"u=bob", "p=pw", "t=abc&s=xyz", "u=bob&p=pw"} {
		for _, inHeader := range []bool{true, false} {
			code, body := keyCall(t, s, key, inHeader, "GET", "/rest/ping?f=json&"+extra, "")
			sr, _ := body["subsonic-response"].(map[string]any)
			e, _ := sr["error"].(map[string]any)
			if code != http.StatusOK || e["code"] != 43.0 {
				t.Errorf("key (header %v) with %s: %d %v", inHeader, extra, code, body)
			}
		}
	}
	// The key alone still works.
	code, body := keyCall(t, s, key, true, "GET", "/rest/ping?f=json", "")
	if sr, _ := body["subsonic-response"].(map[string]any); code != http.StatusOK || sr["status"] != "ok" {
		t.Fatalf("key alone: %d %v", code, body)
	}
	// On the JSON API the conflict is a coded refusal.
	code, body = keyCall(t, s, key, true, "GET", "/api/analysis?id=so-1&u=bob", "")
	if code != http.StatusUnauthorized || body["error"] != "credentials_conflict" {
		t.Fatalf("JSON API conflict: %d %v", code, body)
	}
}
