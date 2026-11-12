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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
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
		writeAPIError(w, http.StatusTooManyRequests, "too many failed sign-in attempts; wait before trying again")
		return
	}
	u, err := users.ByName(r.Context(), s.Pool, in.Username)
	if err != nil {
		// Unknown username: spend the same time as a wrong password.
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
		writeAPIError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	s.auth.succeed(ip)
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"token": token, "username": u.Username, "role": u.Role})
}
