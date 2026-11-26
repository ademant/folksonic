package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// JSON endpoints for accounts, API keys and device pairing. These are not part
// of the Subsonic API, so they use a plain JSON body and status codes.

// writeAPIError answers a JSON API error with the generic code for its
// status (statusCodes) and msg as the message: {"error": "<code>",
// "message": "<text>"}, the one error shape of the API (#194). Use
// writeAPIErrorCode for an error clients need to tell apart.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeAPIErrorCode(w, status, statusCode(status), msg)
}

// writeAPIErrorCode answers a JSON API error with a specific code.
func writeAPIErrorCode(w http.ResponseWriter, status int, code, msg string) {
	challenge(w, status)
	writeJSON(w, status, J{"error": code, "message": msg})
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
	// A device proof is not a session either: a paired app may not manage the account.
	if key := apiKeyOf(r); key != "" || r.Header.Get("DPoP") != "" {
		writeCoded(w, http.StatusForbidden, users.CodeSessionRequired)
		return users.User{}, false
	}
	return u, true
}

// decodeBody reads a JSON request body into v (#195). A body over the size
// limit is 413 too_large, not 400; anything after the JSON value is
// invalid_json; a Content-Type other than JSON is 415. A missing Content-Type
// is accepted, since simple clients and curl often leave it out.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if !jsonContentType(r) {
		writeAPIError(w, http.StatusUnsupportedMediaType, "send the body as application/json")
		return false
	}
	if err := decodeOneJSON(r.Body, v); err != nil {
		if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeAPIErrorCode(w, http.StatusRequestEntityTooLarge, codeTooLarge,
				"request body over "+strconv.FormatInt(mbe.Limit, 10)+" bytes")
			return false
		}
		writeAPIErrorCode(w, http.StatusBadRequest, codeInvalidJSON, "invalid JSON body")
		return false
	}
	return true
}

// decodeOneJSON decodes exactly one JSON value from body.
func decodeOneJSON(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("data after the JSON value")
		}
		return err
	}
	return nil
}

// jsonContentType accepts application/json (with parameters such as
// charset), any +json type, and a missing Content-Type.
func jsonContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// serverURL is the address clients should use: the configured base_url, or,
// when none is configured, the host the request arrived on, with https when it
// came over TLS or through a trusted proxy that says so (#183). The Host
// header is the client's, so a security-relevant address needs base_url; the
// server warns at startup when it is unset.
func (s *State) serverURL(r *http.Request) string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	scheme := "http://"
	if isHTTPS(r, s.trusted) {
		scheme = "https://"
	}
	return scheme + r.Host + s.BasePath
}

// nullableString is nil for an empty string, so a JSON field is null, not "".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PairURI is the payload encoded in the pairing QR code. pin is the server's
// certificate pin (sha256/…) when it serves HTTPS with tls_cert; the app stores
// it and refuses any other certificate. Empty pin leaves it out.
func PairURI(server, token, pin string) string {
	v := url.Values{"server": {server}, "token": {token}}
	if pin != "" {
		v.Set("pin", pin)
	}
	return "tinysonic://pair?" + v.Encode()
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
	// Registration attempts count against the sign-in limiter of the
	// address (#190): a taken username still says so, but probing many names
	// (or guessing invitation codes) is slowed and then refused with 429.
	ip := s.clientIP(r)
	if ok, retry := s.auth.allow(ip); !ok {
		logAuthFailure(ip, "rate-limit", r.URL.Path)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(retry)))
		writeAPIErrorCode(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts; wait before trying again")
		return
	}
	if in.Invite != "" {
		s.registerWithInvite(w, r, in.Username, in.Password, in.Invite)
		return
	}
	if !s.AllowRegistration {
		writeAPIErrorCode(w, http.StatusForbidden, codeRegistrationDisabled, "registration is disabled")
		return
	}
	err := users.CheckPassword(in.Password)
	if err == nil {
		_, err = users.Create(r.Context(), s.Pool, in.Username, in.Password, users.RoleUser, users.StatusPending)
	}
	switch {
	case errors.Is(err, users.ErrExists):
		time.Sleep(s.auth.fail(ip))
		writeAPIErrorCode(w, http.StatusConflict, codeUsernameTaken, err.Error())
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
	default:
		writeJSON(w, http.StatusAccepted, J{"status": users.StatusPending, "message": "awaiting approval"})
	}
}

// registerWithInvite works regardless of allow_registration: the code is the
// permission. The new account gets a session, like a password sign-in.
func (s *State) registerWithInvite(w http.ResponseWriter, r *http.Request, username, password, code string) {
	u, err := users.RegisterWithInvite(r.Context(), s.Pool, code, username, password)
	ip := s.clientIP(r)
	switch {
	case errors.Is(err, users.ErrInviteExpired):
		time.Sleep(200*time.Millisecond + s.auth.fail(ip)) // slow down online guessing
		writeCoded(w, http.StatusForbidden, users.CodeInviteExpired)
		return
	case errors.Is(err, users.ErrInviteInvalid):
		time.Sleep(200*time.Millisecond + s.auth.fail(ip)) // slow down online guessing
		writeCoded(w, http.StatusForbidden, users.CodeInviteInvalid)
		return
	case errors.Is(err, users.ErrExists):
		time.Sleep(s.auth.fail(ip))
		writeAPIErrorCode(w, http.StatusConflict, codeUsernameTaken, err.Error())
		return
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
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
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
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
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
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
			// The key may fetch analysis jobs and upload results (#163).
			"analysisImport": k.AnalysisImport,
		})
	}
	// canGrantAnalysisImport tells the account page to offer the grant.
	writeJSON(w, http.StatusOK, J{"keys": out, "canGrantAnalysisImport": u.Role == users.RoleLibrarian})
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
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
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
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	server := s.serverURL(r)
	writeJSON(w, http.StatusCreated, J{
		"token":     token,
		"uri":       PairURI(server, token, s.tlsPin),
		"link":      PairLink(server, token),
		"pin":       nullableString(s.tlsPin),
		"expiresIn": int(users.PairingTTL / time.Second),
	})
}

// APIPairComplete is unauthenticated: the single-use token is the credential.
func (s *State) APIPairComplete(w http.ResponseWriter, r *http.Request) {
	var in devicePairRequest
	if !decodeBody(w, r, &in) {
		return
	}
	// A pairing that sends the app's public key makes a device that waits for
	// approval (devices.go). The legacy form, without a key, still returns an API
	// key for apps that predate device keys; see README.
	if in.JWK != nil {
		s.completeDevicePairing(w, r, in)
		return
	}
	key, u, err := users.RedeemPairingToken(r.Context(), s.Pool, in.Token, in.Device)
	if errors.Is(err, users.ErrPairingTokenUsed) {
		writeCoded(w, http.StatusConflict, users.CodePairingTokenUsed)
		return
	}
	if errors.Is(err, users.ErrPairingExpired) {
		writeCoded(w, http.StatusUnauthorized, users.CodePairingExpired)
		return
	}
	if errors.Is(err, users.ErrNotFound) {
		writeCoded(w, http.StatusUnauthorized, users.CodePairingInvalid)
		return
	}
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{
		"apiKey":   key,
		"username": u.Username,
		"server":   s.serverURL(r),
	})
}
