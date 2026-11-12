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
)

// refusalMessages says, for people, what each code means. Clients decode the
// code and show their own text when they want to.
var refusalMessages = map[string]string{
	users.CodeCredentialsRequired: "Sign in or send an API key.",
	users.CodeKeyUnknown:          "The API key is not recognised. Pair the device again.",
	users.CodeKeyRevoked:          "The API key was revoked. Pair the device again.",
	users.CodeKeyIdle:             "The API key was revoked because it was not used for a long time. Pair the device again.",
	users.CodeKeyExpired:          "The API key has expired. Pair the device again.",
	users.CodeSessionUnknown:      "The session is not recognised. Sign in again.",
	users.CodeSessionExpired:      "The session has expired. Sign in again.",
	users.CodeUserDisabled:        "The account is disabled.",
	users.CodeScopeForbidden:      "This key may not be used for this request.",
	users.CodeSessionRequired:     "This action needs a signed-in session. Sign in with a password or passkey and try again.",
	users.CodePairingExpired:      "The pairing code has expired. Create a new one.",
	users.CodePairingInvalid:      "The pairing code is not valid or was already used.",
	users.CodeInviteExpired:       "The invitation code has expired.",
	users.CodeInviteInvalid:       "The invitation code is not valid.",
	codePasswordBad:               "Wrong username or password.",
	codeRateLimited:               "Too many failed sign-in attempts. Wait before trying again.",
	codeHTTPSRequired:             "HTTPS is required. Use the address in base_url.",
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
	writeJSON(w, status, J{"error": code, "message": refusalMessage(code)})
}

// writeRefusal answers a request whose credentials were refused, with the code
// recorded when they were checked. A request with no credentials at all gets
// credentials_required.
func (s *State) writeRefusal(w http.ResponseWriter, r *http.Request) {
	code := ""
	if l := reqLogFrom(r.Context()); l != nil {
		code = l.refusal
	}
	if code == "" {
		code = users.CodeCredentialsRequired
	}
	writeCoded(w, http.StatusUnauthorized, code)
}
