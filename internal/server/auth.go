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

	if key, fromHeader := apiKeyOf(r); key != "" {
		u, info, err := users.LookupAPIKeyInfo(ctx, s.Pool, key)
		if err != nil {
			reason, keyID, label := users.APIKeyProblem(ctx, s.Pool, key)
			s.logRefused(r, reason, keyID, label)
			return users.User{}, false
		}
		// A worker key works only in the X-Api-Key header and only on the
		// worker endpoints; everywhere else it is no key at all.
		if info.Scope == users.ScopeWorker && !(fromHeader && strings.HasPrefix(r.URL.Path, "/api/worker/")) {
			s.logRefused(r, "worker-key-scope", info.ID, "")
			return users.User{}, false
		}
		return u, true
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		u, err := users.LookupSession(ctx, s.Pool, tok)
		if err != nil {
			s.logRefused(r, users.SessionProblem(ctx, s.Pool, tok), 0, "")
			return users.User{}, false
		}
		return u, true
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
		logAuthFailure(ip, "rate-limit", r.URL.Path)
		return users.User{}, false
	}
	u, err := users.ByName(ctx, s.Pool, name)
	if err != nil {
		// Hash anyway, so an unknown username takes as long as a wrong password.
		passwords.Burn(q.Get("p"))
		s.authFailed(ip, "unknown-user", r.URL.Path)
		return users.User{}, false
	}
	if u.Status != users.StatusActive {
		passwords.Burn(q.Get("p"))
		s.authFailed(ip, "not-active", r.URL.Path)
		return users.User{}, false
	}
	if !authCheck(u.Username, u.PasswordHash, name, q.Get("p")) {
		s.authFailed(ip, "password", r.URL.Path)
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
	logCredentialFailure(s.clientIP(r), reason, r.URL.Path, extra)
}

// authFailed records a failed password attempt and makes the caller wait the
// progressive delay before it is answered. It is the same for every failure
// reason, so the timing does not show whether a username exists.
func (s *State) authFailed(ip, reason, path string) {
	logAuthFailure(ip, reason, path)
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
