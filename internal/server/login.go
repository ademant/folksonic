package server

import (
	_ "embed"
	"net/http"
	"strconv"

	"github.com/tsirysndr/tinysonic/internal/passwords"
	"github.com/tsirysndr/tinysonic/internal/users"
)

//go:embed static/login.html
var loginPage []byte

// ServeLoginPage serves the minimal sign-in page for the web UI.
func (s *State) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
	_, _ = w.Write(loginPage)
}

// APILogin signs in with username and password and returns a session token.
// It is the password fallback for the web UI; passkeys are preferred.
func (s *State) APILogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	ip := s.clientIP(r)
	if ok, retry := s.auth.allow(ip); !ok {
		logAuthFailure(ip, "rate-limit", r.URL.Path)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(retry)))
		writeAPIErrorCode(w, http.StatusTooManyRequests, codeRateLimited, "too many failed sign-in attempts; wait before trying again")
		return
	}
	if !passwords.TryAcquire() {
		s.auth.refund(ip)
		logAuthFailure(ip, "busy", r.URL.Path)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(busyRetry)))
		writeCoded(w, http.StatusTooManyRequests, codeBusy)
		return
	}
	defer passwords.Release()
	u, err := users.ByName(r.Context(), s.Pool, in.Username)
	if err != nil || u.PasswordHash == "" {
		// Unknown username, or a passkey-only account without a hash: spend
		// the same time as a wrong password, so the answer's timing does not
		// tell which accounts exist or have a password (#199).
		passwords.Burn(in.Password)
	}
	// A passkey-only account has no hash, and an empty password never matches.
	match := err == nil && passwords.Verify(u.PasswordHash, in.Password)
	if !match || u.Status != users.StatusActive {
		reason := "password"
		switch {
		case err != nil:
			reason = "unknown-user"
		case u.Status != users.StatusActive:
			reason = "not-active"
		}
		s.authFailed(r, ip, reason, r.URL.Path)
		writeAPIErrorCode(w, http.StatusUnauthorized, codePasswordBad, "wrong username or password")
		return
	}
	s.auth.succeed(ip)
	token, err := s.newSession(r, u.ID)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"token": token, "username": u.Username, "role": u.Role})
}
