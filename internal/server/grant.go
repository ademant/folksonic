package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// The device authorization grant for our own apps (#93). The app starts a grant,
// the user approves its code on the account page, and the app receives a DPoP
// bound access token (15 minutes) and a refresh token that rotates. Subsonic
// clients are not affected: they keep apiKeyAuthentication.

// maxPendingDeviceCodes bounds the grants waiting for approval, so anonymous
// requests cannot fill the table.
const maxPendingDeviceCodes = 500

// grantDeviceCode is the RFC 8628 grant type of the token endpoint.
const grantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

// APIDeviceAuthorize starts a grant (RFC 8628 section 3.1). The body may name
// the device, for the account page.
func (s *State) APIDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.throttled(w, r, &s.beginLimit) {
		return
	}
	var in struct {
		Label string `json:"label"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &in) {
		return
	}
	now := time.Now()
	pending, err := users.PendingDeviceCodes(r.Context(), s.Pool, now)
	if err != nil {
		logging.Errorf("device authorize: %v", err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	if pending >= maxPendingDeviceCodes {
		writeAPIError(w, http.StatusServiceUnavailable, "too many devices are waiting for approval; try again later")
		return
	}
	auth, err := users.CreateDeviceAuth(r.Context(), s.Pool, in.Label, now)
	if err != nil {
		logging.Errorf("device authorize: %v", err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{
		"device_code":      auth.DeviceCode,
		"user_code":        auth.UserCode,
		"verification_uri": s.accountURL(r),
		"expires_in":       auth.ExpiresIn,
		"interval":         auth.Interval,
	})
}

// accountURL is where the user types the code: the account page.
// accountURL is the absolute address of the account page, which RFC 8628
// requires for verification_uri (#188): base_url, or the address the request
// came to when base_url is unset (serverURL).
func (s *State) accountURL(r *http.Request) string {
	return s.serverURL(r) + "/account"
}

// APIDeviceApprove gives a pending code to the signed-in user (session only).
func (s *State) APIDeviceApprove(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var in struct {
		UserCode string `json:"user_code"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	err := users.ApproveUserCode(r.Context(), s.Pool, u.ID, in.UserCode, time.Now())
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "that code is not waiting for approval; check it on the app and try again")
		return
	}
	if err != nil {
		logging.Errorf("device approve: %v", err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"approved": true})
}

// APIDeviceRevoke ends every device grant of the signed-in user (session only).
func (s *State) APIDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	n, err := users.RevokeUserGrants(r.Context(), s.Pool, u.ID, time.Now())
	if err != nil {
		logging.Errorf("device revoke: %v", err)
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"revoked": n})
}

// APIDeviceToken is the token endpoint (RFC 8628 section 3.4). It needs a DPoP
// proof: the tokens are bound to the key that signed it.
func (s *State) APIDeviceToken(w http.ResponseWriter, r *http.Request) {
	// Per-address limit before any lookup (#178). RFC 8628 clients back off
	// on slow_down, so the limit answers in that shape.
	if ok, retry := s.pollLimit.allow(s.clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(retry)))
		writeGrantError(w, "slow_down")
		return
	}
	if r.FormValue("grant_type") != grantDeviceCode {
		writeGrantError(w, "unsupported_grant_type")
		return
	}
	deviceCode := r.FormValue("device_code")
	now := time.Now()
	if deviceCode == "" {
		writeGrantError(w, "invalid_request")
		return
	}
	// The code must be one we issued before its proof is recorded, so unknown
	// codes cannot fill the replay store (#111).
	known, err := users.DeviceCodeKnown(r.Context(), s.Pool, deviceCode, now)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	jkt, jti, ok := s.grantProof(w, r)
	if !ok {
		return
	}
	if !known {
		writeGrantError(w, "invalid_grant")
		return
	}
	if !s.proofFresh(r, jkt, jti) {
		writeGrantError(w, "invalid_dpop_proof")
		return
	}
	toks, err := users.ExchangeDeviceCode(r.Context(), s.Pool, deviceCode, jkt, now)
	if err != nil {
		writeGrantError(w, grantErrorCode(err))
		return
	}
	writeTokens(w, toks)
}

// APIDeviceRefresh rotates a refresh token (RFC 6749 section 6). The proof must
// come from the key the grant is bound to.
func (s *State) APIDeviceRefresh(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("grant_type") != "refresh_token" {
		writeGrantError(w, "unsupported_grant_type")
		return
	}
	refresh := r.FormValue("refresh_token")
	if refresh == "" {
		writeGrantError(w, "invalid_request")
		return
	}
	known, err := users.GrantKnown(r.Context(), s.Pool, refresh)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	jkt, jti, ok := s.grantProof(w, r)
	if !ok {
		return
	}
	if !known {
		writeGrantError(w, "invalid_grant")
		return
	}
	if !s.proofFresh(r, jkt, jti) {
		writeGrantError(w, "invalid_dpop_proof")
		return
	}
	toks, err := users.RefreshTokens(r.Context(), s.Pool, refresh, jkt, time.Now())
	if errors.Is(err, users.ErrRefreshReused) {
		logging.Warnf("device grant revoked: a rotated refresh token was used again ip=%s", s.clientIP(r))
		writeGrantError(w, "invalid_grant")
		return
	}
	if err != nil {
		writeGrantError(w, grantErrorCode(err))
		return
	}
	writeTokens(w, toks)
}

// grantProof verifies the DPoP proof of a token request and returns the key
// thumbprint and the proof's jti. It writes the error and returns false when
// the proof is missing or not valid for this request.
func (s *State) grantProof(w http.ResponseWriter, r *http.Request) (jkt, jti string, ok bool) {
	proof := r.Header.Get("DPoP")
	if proof == "" {
		writeGrantError(w, "invalid_dpop_proof")
		return "", "", false
	}
	jkt, jti, ok = s.checkProof(r, proof)
	if !ok {
		writeGrantError(w, "invalid_dpop_proof")
		return "", "", false
	}
	return jkt, jti, true
}

// grantErrorCode maps the users package's errors to the RFC error codes.
func grantErrorCode(err error) string {
	switch {
	case errors.Is(err, users.ErrAuthorizationPending):
		return "authorization_pending"
	case errors.Is(err, users.ErrSlowDown):
		return "slow_down"
	case errors.Is(err, users.ErrDeviceCodeExpired):
		return "expired_token"
	default:
		return "invalid_grant"
	}
}

// writeGrantError answers a token request with the RFC error shape.
func writeGrantError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, J{"error": code})
}

// writeTokens answers a successful token request. The token type is DPoP, so
// clients know to send the proof with the access token.
func writeTokens(w http.ResponseWriter, t users.Tokens) {
	writeJSON(w, http.StatusOK, J{
		"access_token":  t.AccessToken,
		"token_type":    "DPoP",
		"expires_in":    t.ExpiresIn,
		"refresh_token": t.RefreshToken,
	})
}

// verifyGrant admits a request with an access token from a device grant. The
// request must carry a DPoP proof from the key the token was issued to. Such a
// request is a device request: it may read and stream the library, but not
// manage the account (requireSession refuses any DPoP header).
func (s *State) verifyGrant(r *http.Request, access string) (users.User, bool) {
	proof := r.Header.Get("DPoP")
	if proof == "" {
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return users.User{}, false
	}
	jkt, jti, ok := s.checkProof(r, proof)
	if !ok {
		return users.User{}, false
	}
	u, err := users.LookupAccessToken(r.Context(), s.Pool, access, jkt, time.Now())
	switch {
	case errors.Is(err, users.ErrTokenExpired):
		s.logRefused(r, users.CodeTokenExpired, 0, "")
		return users.User{}, false
	case err != nil:
		s.logRefused(r, users.CodeTokenInvalid, 0, "")
		return users.User{}, false
	}
	if !s.proofFresh(r, jkt, jti) {
		return users.User{}, false
	}
	return u, true
}

// requireSessionOrGrant admits a signed-in session or a device grant, for the
// one request a device grant may make beyond the library: minting a signed
// stream link. API keys and paired devices are refused.
func (s *State) requireSessionOrGrant(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return u, false
	}
	grant := strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ")
	if apiKeyOf(r) != "" || (r.Header.Get("DPoP") != "" && !grant) {
		writeCoded(w, http.StatusForbidden, users.CodeSessionRequired)
		return users.User{}, false
	}
	return u, true
}
