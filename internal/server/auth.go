package server

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// authenticate resolves the caller to an active account. It accepts, in order:
// an apiKey parameter (our Android app), a Bearer session token (web sign-in),
// or Subsonic credentials (u with t+s, or p). A presented but invalid API key
// or session does not fall through to the other methods.
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
		return users.User{}, false
	}
	if !authCheck(u.Username, u.Password, name, q.Get("p"), q.Get("t"), q.Get("s")) {
		return users.User{}, false
	}
	return u, true
}

// authCheck returns true when the given Subsonic-style params authenticate.
// Either token auth (t with s) or plaintext auth (p, optionally "enc:"-hex
// encoded). Every secret comparison is constant-time so response timing does
// not reveal how much of a password or token matched.
func authCheck(username, password, u, p, t, s string) bool {
	if u == "" || subtle.ConstantTimeCompare([]byte(u), []byte(username)) != 1 {
		return false
	}
	if t != "" && s != "" {
		sum := md5.Sum([]byte(password + s))
		want := hex.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(strings.ToLower(t)), []byte(want)) == 1
	}
	if p != "" {
		decoded := p
		if rest, ok := strings.CutPrefix(p, "enc:"); ok {
			if b, err := hex.DecodeString(rest); err == nil {
				decoded = string(b)
			}
		}
		return subtle.ConstantTimeCompare([]byte(decoded), []byte(password)) == 1
	}
	return false
}
