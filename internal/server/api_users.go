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
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
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

// APIRegister creates a pending account. Disabled unless allow_registration is set.
func (s *State) APIRegister(w http.ResponseWriter, r *http.Request) {
	if !s.AllowRegistration {
		writeAPIError(w, http.StatusForbidden, "registration is disabled")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &in) {
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

func (s *State) APIListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	all, err := users.List(r.Context(), s.Pool)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(all))
	for _, u := range all {
		out = append(out, userJSON(u))
	}
	writeJSON(w, http.StatusOK, J{"users": out})
}

// APIApprove activates a pending account. Any active user may approve.
func (s *State) APIApprove(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := users.ByID(r.Context(), s.Pool, id)
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.Status != users.StatusPending {
		writeAPIError(w, http.StatusConflict, "user is not pending")
		return
	}
	if err := users.SetStatus(r.Context(), s.Pool, id, users.StatusActive, caller.ID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	target.Status = users.StatusActive
	writeJSON(w, http.StatusOK, userJSON(target))
}

// APIReject deletes a pending registration.
func (s *State) APIReject(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := users.ByID(r.Context(), s.Pool, id)
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.Status != users.StatusPending {
		writeAPIError(w, http.StatusConflict, "user is not pending")
		return
	}
	if err := users.Delete(r.Context(), s.Pool, id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "rejected"})
}

// APICreateKey issues an API key for the caller. The key is returned once.
func (s *State) APICreateKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Label string `json:"label"`
		// ExpiresDays limits the key's lifetime. Zero means no expiry.
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
	key, err := users.CreateAPIKey(r.Context(), s.Pool, u.ID, in.Label, ttl)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	resp := J{"apiKey": key, "label": in.Label, "expiresAt": nil}
	if ttl > 0 {
		resp["expiresAt"] = time.Now().Add(ttl).UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *State) APIListKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
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
	u, ok := s.requireUser(w, r)
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
	u, ok := s.requireUser(w, r)
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
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusUnauthorized, "invalid or expired pairing token")
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
