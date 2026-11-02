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

// Self-registration creates a pending account, which another active user
// must approve before it can sign in.
func TestRegistrationNeedsApproval(t *testing.T) {
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

	// Bob cannot sign in yet.
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?u=bob&p=pw", nil)); ok {
		t.Fatal("pending user authenticated")
	}

	// The admin approves with an API key.
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	key, _ := users.CreateAPIKey(ctx, s.Pool, admin.ID, "test", 0)
	req = httptest.NewRequest(http.MethodPost, "/api/users/"+itoa(bob.ID)+"/approve", nil)
	req.SetPathValue("id", itoa(bob.ID))
	req.URL.RawQuery = "apiKey=" + key
	rec = httptest.NewRecorder()
	s.APIApprove(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.authenticate(httptest.NewRequest(http.MethodGet, "/rest/ping?u=bob&p=pw", nil)); !ok {
		t.Fatal("approved user cannot sign in")
	}
}

func TestPendingUserCannotApprove(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	bob, _ := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusPending)
	carol, _ := users.Create(ctx, s.Pool, "carol", "pw", users.RoleUser, users.StatusPending)

	req := httptest.NewRequest(http.MethodPost, "/api/users/"+itoa(carol.ID)+"/approve?u=bob&p=pw", nil)
	req.SetPathValue("id", itoa(carol.ID))
	rec := httptest.NewRecorder()
	s.APIApprove(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pending user approve status %d, want 401", rec.Code)
	}
	_ = bob
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
