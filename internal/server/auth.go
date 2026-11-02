package server

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

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
