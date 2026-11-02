package server

import (
	"crypto/subtle"
	_ "embed"
	"net/http"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

//go:embed static/login.html
var loginPage []byte

// ServeLoginPage serves the minimal sign-in page for the web UI.
func (s *State) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
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
	u, err := users.ByName(r.Context(), s.Pool, in.Username)
	// Compare even when the account is missing, so timing does not reveal which
	// usernames exist.
	match := subtle.ConstantTimeCompare([]byte(in.Password), []byte(u.Password)) == 1
	if err != nil || !match || u.Status != users.StatusActive {
		time.Sleep(200 * time.Millisecond) // slow down online guessing
		writeAPIError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"token": token, "username": u.Username, "role": u.Role})
}
