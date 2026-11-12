package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// JSON endpoints for accounts, API keys and device pairing. These are not part
// of the Subsonic API, so they use a plain JSON body and status codes.

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, J{"error": msg})
}

// requireUser authenticates the caller for an /api request.
func (s *State) requireUser(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	u, ok := s.authenticate(r)
	if !ok {
		s.writeRefusal(w, r)
		return users.User{}, false
	}
	return u, true
}

// requireSession admits only a signed-in session for account and device
// management. An API key may be used for the Subsonic API and the analysis
// reads, but not to create, list or revoke keys, pair devices, manage passkeys
// or identities, or run library jobs: a leaked key must not be able to mint a
// new one. resolveCaller takes the key before any bearer token, so a request
// with an API key is a key request, whatever else it carries.
func (s *State) requireSession(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return u, false
	}
	if key, _ := apiKeyOf(r); key != "" {
		writeCoded(w, http.StatusForbidden, users.CodeSessionRequired)
		return users.User{}, false
	}
	return u, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// serverURL is the address clients should use: the configured base_url, or the
// host the request arrived on when none is configured.
func (s *State) serverURL(r *http.Request) string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "http://" + r.Host
}

// PairURI is the payload encoded in the pairing QR code.
func PairURI(server, token string) string {
	return "tinysonic://pair?" + url.Values{"server": {server}, "token": {token}}.Encode()
}

func userJSON(u users.User) J {
	return J{
		"id":        u.ID,
		"username":  u.Username,
		"role":      u.Role,
		"status":    u.Status,
		"createdAt": u.CreatedAt,
	}
}

// APIRegister creates an account. With an invitation code it is active right
// away and signed in; without one it is pending and needs allow_registration.
func (s *State) APIRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Invite != "" {
		s.registerWithInvite(w, r, in.Username, in.Password, in.Invite)
		return
	}
	if !s.AllowRegistration {
		writeAPIError(w, http.StatusForbidden, "registration is disabled")
		return
	}
	_, err := users.Create(r.Context(), s.Pool, in.Username, in.Password, users.RoleUser, users.StatusPending)
	switch {
	case errors.Is(err, users.ErrExists):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
	default:
		writeJSON(w, http.StatusAccepted, J{"status": users.StatusPending, "message": "awaiting approval"})
	}
}

// registerWithInvite works regardless of allow_registration: the code is the
// permission. The new account gets a session, like a password sign-in.
func (s *State) registerWithInvite(w http.ResponseWriter, r *http.Request, username, password, code string) {
	u, err := users.RegisterWithInvite(r.Context(), s.Pool, code, username, password)
	switch {
	case errors.Is(err, users.ErrInviteExpired):
		time.Sleep(200 * time.Millisecond) // slow down online guessing
		writeCoded(w, http.StatusForbidden, users.CodeInviteExpired)
		return
	case errors.Is(err, users.ErrInviteInvalid):
		time.Sleep(200 * time.Millisecond) // slow down online guessing
		writeCoded(w, http.StatusForbidden, users.CodeInviteInvalid)
		return
	case errors.Is(err, users.ErrExists):
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, J{"status": u.Status, "token": token, "username": u.Username, "role": u.Role})
}

// APICreateKey issues an API key for the caller. The key is returned once.
func (s *State) APICreateKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var in struct {
		Label string `json:"label"`
		// ExpiresDays sets the key's lifetime in days. Zero means the
		// server's default (api_key_ttl_days); every key expires.
		ExpiresDays int `json:"expires_days"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.ExpiresDays < 0 || in.ExpiresDays > 3650 {
		writeAPIError(w, http.StatusBadRequest, "expires_days must be between 0 and 3650")
		return
	}
	ttl := time.Duration(in.ExpiresDays) * 24 * time.Hour
	if ttl <= 0 {
		ttl = users.KeyTTL
	}
	key, err := users.CreateAPIKey(r.Context(), s.Pool, u.ID, in.Label, ttl)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	resp := J{"apiKey": key, "label": in.Label, "expiresAt": time.Now().Add(ttl).UTC().Format(time.RFC3339)}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *State) APIListKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	keys, err := users.ListAPIKeys(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(keys))
	for _, k := range keys {
		out = append(out, J{
			"id":         k.ID,
			"label":      k.Label,
			"prefix":     k.Prefix,
			"createdAt":  k.CreatedAt,
			"lastUsedAt": nsToAny(k.LastUsedAt),
			"expiresAt":  nsToAny(k.ExpiresAt),
		})
	}
	writeJSON(w, http.StatusOK, J{"keys": out})
}

func (s *State) APIRevokeKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := users.RevokeAPIKey(r.Context(), s.Pool, u.ID, id)
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "revoked"})
}

// APIPairStart issues a short-lived pairing token and the QR payload that
// encodes it. The app redeems the token once for a long-lived API key.
func (s *State) APIPairStart(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	token, err := users.CreatePairingToken(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	server := s.serverURL(r)
	writeJSON(w, http.StatusCreated, J{
		"token":     token,
		"uri":       PairURI(server, token),
		"expiresIn": int(users.PairingTTL / time.Second),
	})
}

// APIPairComplete is unauthenticated: the single-use token is the credential.
func (s *State) APIPairComplete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token  string `json:"token"`
		Device string `json:"device"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	key, u, err := users.RedeemPairingToken(r.Context(), s.Pool, in.Token, in.Device)
	if errors.Is(err, users.ErrPairingExpired) {
		writeCoded(w, http.StatusUnauthorized, users.CodePairingExpired)
		return
	}
	if errors.Is(err, users.ErrNotFound) {
		writeCoded(w, http.StatusUnauthorized, users.CodePairingInvalid)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{
		"apiKey":   key,
		"username": u.Username,
		"server":   s.serverURL(r),
	})
}
