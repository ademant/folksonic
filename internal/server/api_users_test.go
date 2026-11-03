package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

func TestRegistrationDisabledByDefault(t *testing.T) {
	s := newTestState(t)
	req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(`{"username":"x","password":"y"}`))
	rec := httptest.NewRecorder()
	s.APIRegister(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 when allow_registration is off", rec.Code)
	}
}

// Self-registration creates a pending account. Only the CLI approves it, so it
// stays pending until an administrator runs `tinysonic user approve`.
func TestRegistrationNeedsCLIApproval(t *testing.T) {
	s := newTestState(t)
	s.AllowRegistration = true
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(`{"username":"bob","password":"pw"}`))
	rec := httptest.NewRecorder()
	s.APIRegister(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("register status %d: %s", rec.Code, rec.Body.String())
	}
	bob, err := users.ByName(ctx, s.Pool, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.Status != users.StatusPending {
		t.Fatalf("new account status %q, want pending", bob.Status)
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?u=bob&p=pw", nil)); ok {
		t.Fatal("pending user authenticated")
	}

	// No API endpoint lists, approves or rejects accounts.
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/users"},
		{http.MethodPost, "/api/users/" + itoa(bob.ID) + "/approve"},
		{http.MethodPost, "/api/users/" + itoa(bob.ID) + "/reject"},
	} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(call.method, call.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status %d, want 404", call.method, call.path, rec.Code)
		}
	}

	// `tinysonic user approve` does exactly this.
	if err := users.SetStatus(ctx, s.Pool, bob.ID, users.StatusActive, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?u=bob&p=pw", nil)); !ok {
		t.Fatal("approved user cannot sign in")
	}
}

func TestPairingTokenRedeemsOnce(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	token, err := users.CreatePairingToken(ctx, s.Pool, admin.ID)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"token":"` + token + `","device":"pixel"}`
	req := httptest.NewRequest(http.MethodPost, "/api/pair/complete", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.APIPairComplete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first redeem status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.APIKey == "" {
		t.Fatalf("no api key in response: %s", rec.Body.String())
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?apiKey="+out.APIKey, nil)); !ok {
		t.Fatal("key from pairing does not authenticate")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/pair/complete", strings.NewReader(body))
	rec = httptest.NewRecorder()
	s.APIPairComplete(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second redeem status %d, want 401", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// An invitation code registers an active, signed-in account even with
// allow_registration off, and only once.
func TestRegistrationWithInvite(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "", "", users.InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	register := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.APIRegister(rec, httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body)))
		return rec
	}

	rec := register(`{"username":"bob","password":"pw","invite":"` + code + `"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct{ Status, Token, Username string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != users.StatusActive || out.Username != "bob" || out.Token == "" {
		t.Fatalf("register response %+v", out)
	}
	if u, err := users.LookupSession(ctx, s.Pool, out.Token); err != nil || u.Username != "bob" {
		t.Fatalf("session from registration: %v, %v", u, err)
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?u=bob&p=pw", nil)); !ok {
		t.Fatal("invited user cannot sign in")
	}

	if rec := register(`{"username":"carol","password":"pw","invite":"` + code + `"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("reused invite status %d, want 403", rec.Code)
	}
	if rec := register(`{"username":"carol","password":"pw","invite":"inv-AAAA-AAAA-AAAA-AAAA"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown invite status %d, want 403", rec.Code)
	}
}
