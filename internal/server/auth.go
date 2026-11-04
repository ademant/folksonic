package server

import (
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/passwords"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// authenticate resolves the caller to an active account. It accepts, in order:
// an apiKey parameter (our Android app), a Bearer session token (web sign-in),
// or Subsonic username and password (u with p, plain or enc:-hex). A presented
// but invalid API key or session does not fall through to the other methods.
// Subsonic token auth (t with s) is not accepted; see usesTokenAuth.
func (s *State) authenticate(r *http.Request) (users.User, bool) {
	ctx := r.Context()
	q := params(r)

	if key := q.Get("apiKey"); key != "" {
		u, err := users.LookupAPIKey(ctx, s.Pool, key)
		return u, err == nil
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		u, err := users.LookupSession(ctx, s.Pool, tok)
		return u, err == nil
	}

	name := q.Get("u")
	if name == "" {
		return users.User{}, false
	}
	u, err := users.ByName(ctx, s.Pool, name)
	if err != nil || u.Status != users.StatusActive {
		// Hash anyway, so an unknown username takes as long as a wrong password.
		passwords.Burn(q.Get("p"))
		return users.User{}, false
	}
	if !authCheck(u.Username, u.PasswordHash, name, q.Get("p")) {
		return users.User{}, false
	}
	return u, true
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
