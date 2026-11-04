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

// beginInvitePasskey runs the begin step and returns the ceremony id and the
// challenge the authenticator must sign, or the HTTP status on failure.
func beginInvitePasskey(t *testing.T, s *State, username, code string) (ceremonyID, challenge string, status int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "invite": code})
	req := httptest.NewRequest(http.MethodPost, "/api/register/passkey/begin", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.APIRegisterPasskeyBegin(rec, req)
	if rec.Code != http.StatusOK {
		return "", "", rec.Code
	}
	var out struct {
		Ceremony string `json:"ceremony"`
		Options  struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Ceremony, out.Options.PublicKey.Challenge, http.StatusOK
}

func finishInvitePasskey(t *testing.T, s *State, ceremonyID, body string) (status int, resp string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/register/passkey/finish?ceremony="+ceremonyID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.APIRegisterPasskeyFinish(rec, req)
	return rec.Code, rec.Body.String()
}

// Sign-up with an invitation and a passkey, no password: the account is active,
// has no password, holds the passkey, and the invite is spent.
func TestPasskeyInviteCreatesPasswordlessAccount(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ceremonyID, challenge, status := beginInvitePasskey(t, s, "carol", code)
	if status != http.StatusOK {
		t.Fatalf("begin status %d", status)
	}
	auth := newSoftAuthenticator(t, "http://localhost:4598", "localhost")
	status, resp := finishInvitePasskey(t, s, ceremonyID, string(auth.registration(t, challenge)))
	if status != http.StatusCreated {
		t.Fatalf("finish status %d: %s", status, resp)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Token == "" {
		t.Fatalf("no session token: %s", resp)
	}

	carol, err := users.ByName(ctx, s.Pool, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if carol.PasswordHash != "" || carol.Status != users.StatusActive || len(carol.WebAuthnHandle) != 16 {
		t.Fatalf("unexpected account: hash=%q status=%s handle=%d bytes", carol.PasswordHash, carol.Status, len(carol.WebAuthnHandle))
	}
	if keys, err := users.ListPasskeys(ctx, s.Pool, carol.ID); err != nil || len(keys) != 1 {
		t.Fatalf("passkey not stored: %v %d", err, len(keys))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Header.Set("Authorization", "Bearer "+out.Token)
	rec := httptest.NewRecorder()
	s.APIListKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session token rejected: %d", rec.Code)
	}

	if _, err := users.InviteRoleFor(ctx, s.Pool, code, "dave"); err != users.ErrInviteInvalid {
		t.Fatalf("invite still usable after redemption: %v", err)
	}
}

// A failed passkey must not create the account or use up the invite, so the
// user can try again with the same code.
func TestPasskeyInviteFailureKeepsInvite(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ceremonyID, challenge, _ := beginInvitePasskey(t, s, "erin", code)
	if status, _ := finishInvitePasskey(t, s, ceremonyID, `{}`); status != http.StatusBadRequest {
		t.Fatalf("bad passkey accepted: status %d", status)
	}
	if _, err := users.ByName(ctx, s.Pool, "erin"); err != users.ErrNotFound {
		t.Fatalf("account created despite failed passkey: %v", err)
	}

	ceremonyID, challenge, _ = beginInvitePasskey(t, s, "erin", code)
	auth := newSoftAuthenticator(t, "http://localhost:4598", "localhost")
	if status, resp := finishInvitePasskey(t, s, ceremonyID, string(auth.registration(t, challenge))); status != http.StatusCreated {
		t.Fatalf("retry failed: %d %s", status, resp)
	}
}

// An invite bound to a username only creates that username.
func TestPasskeyInviteBoundToUsername(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "frank", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, status := beginInvitePasskey(t, s, "gina", code); status != http.StatusForbidden {
		t.Fatalf("other username accepted: status %d", status)
	}
	if _, _, status := beginInvitePasskey(t, s, "frank", code); status != http.StatusOK {
		t.Fatalf("bound username refused: status %d", status)
	}
}

// Passkey-only accounts must not be reachable with an empty password, through
// the web login or through the Subsonic password parameter.
func TestPasswordlessAccountCannotUsePassword(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.RegisterPasskey(ctx, s.Pool, code, "hank", []byte("0123456789abcdef"), `{"id":"x"}`); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"hank","password":""}`))
	rec := httptest.NewRecorder()
	s.APILogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty password login: status %d", rec.Code)
	}
	if authCheck("hank", "", "hank", "enc:") {
		t.Fatal("p=enc: (empty) accepted for a passwordless account")
	}
	if authCheck("hank", "", "hank", "") {
		t.Fatal("empty p accepted for a passwordless account")
	}
}
