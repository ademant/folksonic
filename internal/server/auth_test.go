package server

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
	s := &State{Username: "admin", Password: "s3cret"}
	form := url.Values{"u": {"admin"}, "p": {"s3cret"}, "v": {"1.16.1"}, "c": {"test"}}
	req := httptest.NewRequest(http.MethodPost, "/rest/ping", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Ping(rec, req)
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("POST form auth failed: %s", rec.Body.String())
	}
}

func TestPingRejectsWrongPOSTPassword(t *testing.T) {
	s := &State{Username: "admin", Password: "s3cret"}
	form := url.Values{"u": {"admin"}, "p": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/rest/ping", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Ping(rec, req)
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("wrong password accepted: %s", rec.Body.String())
	}
}
