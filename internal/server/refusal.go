package server

import (
	"net/http"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// Error codes for refusals that are not credential failures from the users
// package. The codes are stable: clients branch on them and show the message.
const (
	codeHTTPSRequired = "https_required"
	codePasswordBad   = "password_invalid"
	codeRateLimited   = "rate_limited"
	codeBusy          = "server_busy"
)

// Codes of JSON API errors that clients may need to tell apart from the
// generic code of their status (#194). The message says the rest.
const (
	codeDatabaseError        = "database_error"
	codeInvalidJSON          = "invalid_json"
	codeLibrarianRequired    = "librarian_required"
	codeRegistrationDisabled = "registration_disabled"
	codePasskeysUnavailable  = "passkeys_unavailable"
	codeNoPasskey            = "no_passkey"
	codeCeremonyExpired      = "ceremony_expired"
	codeSSONotConfigured     = "sso_not_configured"
	codeSSOUnavailable       = "sso_unavailable"
	codeLastSignInMethod     = "last_sign_in_method"
	codeJobRunning           = "job_running"
	codeNoJobRunning         = "no_job_running"
	codeImportRunning        = "import_running"
	codeTooLarge             = "too_large"
	codeToolUnavailable      = "tool_unavailable"
	codeMethodNotAllowed     = "method_not_allowed"
	codeUsernameTaken        = "username_taken"
	codeTooManyPlayers       = "too_many_players"
)

// statusCodes are the generic codes of JSON API errors, by HTTP status.
var statusCodes = map[int]string{
	http.StatusBadRequest:            "bad_request",
	http.StatusUnauthorized:          "unauthorized",
	http.StatusForbidden:             "forbidden",
	http.StatusNotFound:              "not_found",
	http.StatusMethodNotAllowed:      codeMethodNotAllowed,
	http.StatusConflict:              "conflict",
	http.StatusRequestEntityTooLarge: codeTooLarge,
	http.StatusUnsupportedMediaType:  "unsupported_media_type",
	http.StatusTooManyRequests:       codeRateLimited,
	http.StatusInternalServerError:   "internal_error",
	http.StatusServiceUnavailable:    "unavailable",
}

// statusCode is the generic code for an HTTP status.
func statusCode(status int) string {
	if c, ok := statusCodes[status]; ok {
		return c
	}
	if status >= 500 {
		return "internal_error"
	}
	return "bad_request"
}

// refusalMessages says, for people, what each code means. Clients decode the
// code and show their own text when they want to.
var refusalMessages = map[string]string{
	users.CodeCredentialsRequired:    "Sign in or send an API key.",
	users.CodeKeyUnknown:             "The API key is not recognised. Pair the device again.",
	users.CodeKeyRevoked:             "The API key was revoked. Pair the device again.",
	users.CodeKeyIdle:                "The API key was revoked because it was not used for a long time. Pair the device again.",
	users.CodeKeyExpired:             "The API key has expired. Pair the device again.",
	users.CodeSessionUnknown:         "The session is not recognised. Sign in again.",
	users.CodeSessionExpired:         "The session has expired. Sign in again.",
	users.CodeUserDisabled:           "The account is disabled.",
	users.CodeScopeForbidden:         "This key may not be used for this request.",
	users.CodeSessionRequired:        "This action needs a signed-in session. Sign in with a password or passkey and try again.",
	users.CodePairingExpired:         "The pairing code has expired. Create a new one.",
	users.CodePairingInvalid:         "The pairing code is not valid or was already used.",
	users.CodeInviteExpired:          "The invitation code has expired.",
	users.CodeInviteInvalid:          "The invitation code is not valid.",
	users.CodeStreamURLExpired:       "The stream link has expired. Ask for a new one.",
	users.CodeStreamURLInvalid:       "The stream link is not valid for this song, or it was changed.",
	users.CodeProofInvalid:           "The device's signature is not valid for this request. Pair the device again.",
	users.CodeProofReplayed:          "This request was already sent. The app should sign a new request.",
	users.CodePairingPending:         "This pairing waits for confirmation: compare the code on this device with the one on the account page, and confirm on both.",
	users.CodeDeviceUnknown:          "This device was removed from the account. Pair it again.",
	users.CodeTokenExpired:           "The access token has expired. Refresh it with the refresh token.",
	users.CodeTokenInvalid:           "The access token is not valid for this device. Sign in on the device again.",
	users.CodePairingRejected:        "This pairing was rejected. Create a new code on the account page and pair again.",
	users.CodePairingTokenUsed:       "This QR code can no longer be used. Create a new code on the account page.",
	users.CodePairingBusy:            "Too many devices are waiting on this QR code. Create a new code on the account page.",
	users.CodeAnalysisImportRequired: "Analysis jobs and results need a librarian's session, or the API key a librarian allowed to import analysis results on the account page, sent in the X-Api-Key header.",
	users.CodeCredentialsConflict:    "Send either an API key or a username with a password, not both.",
	codePasswordBad:                  "Wrong username or password.",
	codeRateLimited:                  "Too many failed sign-in attempts. Wait before trying again.",
	codeBusy:                         "The server is busy checking passwords. Try again in a moment.",
	codeHTTPSRequired:                "HTTPS is required. Use the address in base_url.",
}

// refusalMessage is the text for code, or a generic one for an unknown code.
func refusalMessage(code string) string {
	if m, ok := refusalMessages[code]; ok {
		return m
	}
	return "The request was refused."
}

// writeCoded answers with a JSON error whose code clients can decode.
func writeCoded(w http.ResponseWriter, status int, code string) {
	challenge(w, status)
	writeJSON(w, status, J{"error": code, "message": refusalMessage(code)})
}

// challenge adds WWW-Authenticate to a 401, which HTTP requires (#193). A
// session token is a Bearer token; API keys go in X-Api-Key, which has no
// scheme of its own.
func challenge(w http.ResponseWriter, status int) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="tinysonic"`)
	}
}

// writeRefusal answers a request whose credentials were refused, with the code
// recorded when they were checked. A request with no credentials at all gets
// credentials_required.
func (s *State) writeRefusal(w http.ResponseWriter, r *http.Request) {
	code := ""
	if l := reqLogFrom(r.Context()); l != nil {
		code = l.refusal
	}
	if credentialsConflict(r) {
		code = users.CodeCredentialsConflict
	}
	if code == "" {
		code = users.CodeCredentialsRequired
	}
	writeCoded(w, http.StatusUnauthorized, code)
}
