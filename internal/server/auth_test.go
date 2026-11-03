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
	salt := "abc123"
	sum := md5.Sum([]byte(pass + salt))
	token := hex.EncodeToString(sum[:])
	enc := hex.EncodeToString([]byte(pass))

	tests := []struct {
		name       string
		u, p, t, s string
		want       bool
	}{
		{"token ok", user, "", token, salt, true},
		{"token uppercase accepted", user, "", strings.ToUpper(token), salt, true},
		{"token wrong salt", user, "", token, "other", false},
		{"plaintext ok", user, pass, "", "", true},
		{"plaintext wrong", user, "nope", "", "", false},
		{"enc hex ok", user, "enc:" + enc, "", "", true},
		{"enc hex wrong", user, "enc:" + hex.EncodeToString([]byte("x")), "", "", false},
		{"wrong user", "mallory", pass, "", "", false},
		{"missing user", "", pass, "", "", false},
		{"no credentials", user, "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := authCheck(user, pass, tc.u, tc.p, tc.t, tc.s); got != tc.want {
				t.Fatalf("authCheck = %v, want %v", got, tc.want)
			}
		})
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
