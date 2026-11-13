package server

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/passwords"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// authenticate resolves the caller to an active account. It accepts, in order:
// an apiKey parameter (our Android app), a Bearer session token (web sign-in),
// or Subsonic username and password (u with p, plain or enc:-hex). A presented
// but invalid API key or session does not fall through to the other methods.
// Subsonic token auth (t with s) is not accepted; see usesTokenAuth.
//
// It records the client name and, on success, the username for the request
// log (noteCaller).
func (s *State) authenticate(r *http.Request) (users.User, bool) {
	u, ok := s.resolveCaller(r)
	name := ""
	if ok {
		name = u.Username
	}
	noteCaller(r, name)
	return u, ok
}

func (s *State) resolveCaller(r *http.Request) (users.User, bool) {
	ctx := r.Context()
	q := params(r)

	if key := apiKeyOf(r); key != "" {
		u, info, err := users.LookupAPIKeyInfo(ctx, s.Pool, key)
		if err != nil {
			reason, keyID, label := users.APIKeyProblem(ctx, s.Pool, key)
			s.logRefused(r, reason, keyID, label)
			return users.User{}, false
		}
		// A scoped key (the analysis worker keys removed in #105) is no key
		// at all: no endpoint accepts a scope any more.
		if info.Scope != "" {
			s.logRefused(r, users.CodeScopeForbidden, info.ID, "")
			return users.User{}, false
		}
		return u, true
	}
	// A paired app signs each request with its key (devices.go). No secret is sent.
	if proof := r.Header.Get("DPoP"); proof != "" {
		return s.verifyDevice(r, proof)
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		u, err := users.LookupSession(ctx, s.Pool, tok)
		if err != nil {
			s.logRefused(r, users.SessionProblem(ctx, s.Pool, tok), 0, "")
			return users.User{}, false
		}
		return u, true
	}

	// A signed stream URL (streamurl.go). A presented signature that fails
	// does not fall through to username and password.
	if q.Get("sig") != "" {
		return s.verifySignedStream(r, q)
	}

	name := q.Get("u")
	if name == "" || s.legacyPasswordOff {
		return users.User{}, false
	}
	ip := s.clientIP(r)
	// Refused before any hashing, so a flood of guesses costs little CPU.
	if ok, retry := s.auth.allow(ip); !ok {
		if l := reqLogFrom(ctx); l != nil {
			l.throttled, l.retryAfter = true, retry
		}
		setRefusal(r, codeRateLimited)
		logAuthFailure(ip, "rate-limit", r.URL.Path)
		return users.User{}, false
	}
	u, err := users.ByName(ctx, s.Pool, name)
	if err != nil {
		// Hash anyway, so an unknown username takes as long as a wrong password.
		passwords.Burn(q.Get("p"))
		s.authFailed(r, ip, "unknown-user", r.URL.Path)
		return users.User{}, false
	}
	if u.Status != users.StatusActive {
		passwords.Burn(q.Get("p"))
		s.authFailed(r, ip, "not-active", r.URL.Path)
		return users.User{}, false
	}
	if !authCheck(u.Username, u.PasswordHash, name, q.Get("p")) {
		s.authFailed(r, ip, "password", r.URL.Path)
		return users.User{}, false
	}
	s.auth.succeed(ip)
	return u, true
}

// logRefused logs a refused API key or session. Unlike a wrong password it is
// not delayed: a 256-bit key cannot be guessed, so the point is visibility. The
// key id and label are logged for a key that exists, never the key itself.
func (s *State) logRefused(r *http.Request, reason string, keyID int64, label string) {
	extra := ""
	if keyID != 0 {
		extra = fmt.Sprintf(" key=%d label=%s", keyID, strconv.Quote(label))
	}
	setRefusal(r, reason)
	logCredentialFailure(s.clientIP(r), reason, r.URL.Path, extra)
}

// setRefusal records the code of a refused credential on the request.
func setRefusal(r *http.Request, code string) {
	if l := reqLogFrom(r.Context()); l != nil {
		l.refusal = code
	}
}

// authFailed records a failed password attempt and makes the caller wait the
// progressive delay before it is answered. It is the same for every failure
// reason, so the timing does not show whether a username exists.
func (s *State) authFailed(r *http.Request, ip, reason, path string) {
	logAuthFailure(ip, reason, path)
	// The refusal code for the answer. A disabled account says so; every other
	// password failure reads the same, so an unknown user is not revealed.
	code := codePasswordBad
	if reason == "not-active" {
		code = users.CodeUserDisabled
	}
	setRefusal(r, code)
	time.Sleep(s.auth.fail(ip))
}

// clientIP is the address failed attempts are attributed to; see clientIP.
func (s *State) clientIP(r *http.Request) string {
	return clientIP(r, s.trusted)
}

// usesTokenAuth reports whether a request tried Subsonic token auth (t and s)
// without a password. Such clients are told the mechanism is unsupported,
// rather than given a wrong-password error they cannot fix.
func usesTokenAuth(r *http.Request) bool {
	q := params(r)
	return (q.Get("t") != "" || q.Get("s") != "") && q.Get("p") == "" && q.Get("apiKey") == ""
}

// writeAuthFailure answers a request that did not authenticate. The Subsonic
// error code tells the client what went wrong: 42 for a removed mechanism,
// 40 for wrong credentials.
func (s *State) writeAuthFailure(w http.ResponseWriter, r *http.Request) {
	if s.legacyPasswordOff && params(r).Get("u") != "" {
		writeError(w, 42, "Password authentication is disabled on this server. Use an API key")
		return
	}
	if l := reqLogFrom(r.Context()); l != nil && l.throttled {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(l.retryAfter)))
		writeErrorStatus(w, http.StatusTooManyRequests, 0, "Too many failed sign-in attempts; wait before trying again")
		return
	}
	if usesTokenAuth(r) {
		writeError(w, 42, "Authentication mechanism not supported. Use API keys or username and password")
		return
	}
	if l := reqLogFrom(r.Context()); l != nil && (l.refusal == users.CodeStreamURLExpired || l.refusal == users.CodeStreamURLInvalid) {
		writeErrorStatus(w, http.StatusForbidden, 40, refusalMessage(l.refusal))
		return
	}
	if l := reqLogFrom(r.Context()); l != nil && l.refusal != "" {
		writeError(w, 40, refusalMessage(l.refusal))
		return
	}
	writeError(w, 40, "Wrong username or password")
}

// authCheck reports whether the Subsonic username and password parameters
// match the account. p is either the plain password or "enc:" followed by its
// hex encoding. A passkey-only account has no hash and never matches.
func authCheck(username, hash, u, p string) bool {
	if u == "" || u != username {
		return false
	}
	decoded := p
	if rest, ok := strings.CutPrefix(p, "enc:"); ok {
		if b, err := hex.DecodeString(rest); err == nil {
			decoded = string(b)
		}
	}
	return passwords.Verify(hash, decoded)
}

// apiKeyOf returns the API key a request presents: in the X-Api-Key header,
// or in the apiKey query parameter that Subsonic clients send.
func apiKeyOf(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return params(r).Get("apiKey")
}
