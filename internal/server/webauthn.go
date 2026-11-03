package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// Passkey (WebAuthn) sign-in. Passkeys need HTTPS (or localhost) and a fixed
// relying-party identity, which comes from base_url. The RP ID must be a
// domain name, not an IP address. When base_url is missing or unusable the
// passkey endpoints answer 503 and password or API-key access keeps working.

// ceremonyTTL bounds how long a registration or login challenge stays valid.
const ceremonyTTL = 5 * time.Minute

type ceremony struct {
	session webauthn.SessionData
	userID  int64
	expires time.Time
	// Set only for passkey sign-up with an invitation. No account exists yet,
	// so it is created when the passkey has been verified.
	invite   string
	username string
	handle   []byte
}

// ceremonyStore holds pending challenges in memory. They are short-lived, so a
// restart simply asks the user to try again.
type ceremonyStore struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func (c *ceremonyStore) put(id string, cer ceremony) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	c.m[id] = cer
}

// take returns and removes a challenge; each one can be used once.
func (c *ceremonyStore) take(id string) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[id]
	delete(c.m, id)
	return cer, ok && time.Now().Before(cer.expires)
}

// waUser adapts an account and its passkeys to the library's User interface.
type waUser struct {
	u     users.User
	creds []webauthn.Credential
	ids   []int64 // row IDs, parallel to creds
}

func (w waUser) WebAuthnID() []byte {
	if len(w.u.WebAuthnHandle) > 0 {
		return w.u.WebAuthnHandle
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(w.u.ID))
	return b[:]
}
func (w waUser) WebAuthnName() string                       { return w.u.Username }
func (w waUser) WebAuthnDisplayName() string                { return w.u.Username }
func (w waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func newCeremonyID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *State) loadWAUser(ctx context.Context, u users.User) (waUser, error) {
	recs, err := users.ListPasskeys(ctx, s.Pool, u.ID)
	if err != nil {
		return waUser{}, err
	}
	out := waUser{u: u}
	for _, r := range recs {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(r.JSON), &c); err != nil {
			return waUser{}, fmt.Errorf("passkey %d: %w", r.ID, err)
		}
		out.creds = append(out.creds, c)
		out.ids = append(out.ids, r.ID)
	}
	return out, nil
}

// newWebAuthn builds the relying party from base_url, or returns nil when it
// is not configured.
func newWebAuthn(baseURL string) (*webauthn.WebAuthn, error) {
	if baseURL == "" {
		return nil, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("base_url %q is not an absolute URL", baseURL)
	}
	origin := u.Scheme + "://" + u.Host
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "tinysonic",
		RPID:          u.Hostname(),
		RPOrigins:     []string{origin},
	})
}

func (s *State) requireWebAuthn(w http.ResponseWriter) bool {
	if s.webAuthn == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "passkeys are unavailable: set base_url to a domain name (see server log)")
		return false
	}
	return true
}

// APIPasskeyRegisterBegin starts enrolling a passkey for the signed-in user.
func (s *State) APIPasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	wu, err := s.loadWAUser(r.Context(), caller)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	options, session, err := s.webAuthn.BeginRegistration(wu)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := newCeremonyID()
	s.ceremonies.put(id, ceremony{session: *session, userID: caller.ID, expires: time.Now().Add(ceremonyTTL)})
	writeJSON(w, http.StatusOK, J{"ceremony": id, "options": options})
}

// APIPasskeyRegisterFinish stores the passkey created by the browser or app.
// The request body is the credential response from navigator.credentials.create.
func (s *State) APIPasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	cer, ok := s.ceremonies.take(r.URL.Query().Get("ceremony"))
	if !ok || cer.userID != caller.ID {
		writeAPIError(w, http.StatusBadRequest, "registration expired or not yours; start again")
		return
	}
	wu, err := s.loadWAUser(r.Context(), caller)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	cred, err := s.webAuthn.FinishRegistration(wu, cer.session, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "passkey registration failed: "+err.Error())
		return
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "encoding error")
		return
	}
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if len(label) > maxPasskeyLabel {
		label = label[:maxPasskeyLabel]
	}
	if err := users.AddPasskey(r.Context(), s.Pool, caller.ID, string(raw), label); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, J{"status": "passkey added"})
}

// APIPasskeyLoginBegin starts a passkey sign-in for the named account.
func (s *State) APIPasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	target, err := users.ByName(r.Context(), s.Pool, in.Username)
	if err != nil || target.Status != users.StatusActive {
		writeAPIError(w, http.StatusUnauthorized, "no passkey for this account")
		return
	}
	wu, err := s.loadWAUser(r.Context(), target)
	if err != nil || len(wu.creds) == 0 {
		writeAPIError(w, http.StatusUnauthorized, "no passkey for this account")
		return
	}
	options, session, err := s.webAuthn.BeginLogin(wu)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := newCeremonyID()
	s.ceremonies.put(id, ceremony{session: *session, userID: target.ID, expires: time.Now().Add(ceremonyTTL)})
	writeJSON(w, http.StatusOK, J{"ceremony": id, "options": options})
}

// APIPasskeyLoginFinish verifies the signed challenge and issues a session token
// for the Authorization: Bearer header.
func (s *State) APIPasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	cer, ok := s.ceremonies.take(r.URL.Query().Get("ceremony"))
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "sign-in expired; start again")
		return
	}
	target, err := users.ByID(r.Context(), s.Pool, cer.userID)
	if err != nil || target.Status != users.StatusActive {
		writeAPIError(w, http.StatusUnauthorized, "sign-in failed")
		return
	}
	wu, err := s.loadWAUser(r.Context(), target)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	cred, err := s.webAuthn.FinishLogin(wu, cer.session, r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "sign-in failed")
		return
	}
	s.storeUpdatedPasskey(r.Context(), wu, cred)

	token, err := users.CreateSession(r.Context(), s.Pool, target.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"token": token, "username": target.Username, "role": target.Role})
}

// storeUpdatedPasskey saves the credential's new signature counter after login.
func (s *State) storeUpdatedPasskey(ctx context.Context, wu waUser, used *webauthn.Credential) {
	for i, c := range wu.creds {
		if string(c.ID) != string(used.ID) {
			continue
		}
		raw, err := json.Marshal(used)
		if err == nil {
			_ = users.UpdatePasskey(ctx, s.Pool, wu.ids[i], string(raw))
		}
		return
	}
}

// APILogout ends the session behind a Bearer token.
func (s *State) APILogout(w http.ResponseWriter, r *http.Request) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		_ = users.DeleteSession(r.Context(), s.Pool, tok)
	}
	writeJSON(w, http.StatusOK, J{"status": "signed out"})
}
