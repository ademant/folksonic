package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// Session management (#189): the account page lists the caller's sessions,
// ends one of them, or all ("sign out everywhere"). Only a session may do
// this, not an API key or a device.

// newSession signs userID in and labels the session with the client it came
// from, for the session list.
func (s *State) newSession(r *http.Request, userID int64) (string, error) {
	return users.CreateSession(r.Context(), s.Pool, userID, sessionLabel(r.UserAgent()))
}

// sessionLabel names a client from its User-Agent: the browser or app and
// the system, as far as they are recognisable, cut to 80 characters.
func sessionLabel(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "unknown client"
	}
	browser := ""
	for _, b := range []string{"Firefox", "Edg", "OPR", "Chrome", "Safari", "DanceSonic", "okhttp", "curl"} {
		if strings.Contains(ua, b+"/") {
			browser = map[string]string{"Edg": "Edge", "OPR": "Opera"}[b]
			if browser == "" {
				browser = b
			}
			break
		}
	}
	system := ""
	for _, o := range []string{"Android", "iPhone", "iPad", "Windows", "Mac OS X", "Linux"} {
		if strings.Contains(ua, o) {
			system = map[string]string{"Mac OS X": "macOS"}[o]
			if system == "" {
				system = o
			}
			break
		}
	}
	label := strings.TrimSpace(browser + " on " + system)
	switch {
	case browser == "" && system == "":
		label = ua
	case system == "":
		label = browser
	case browser == "":
		label = system
	}
	if len(label) > 80 {
		label = label[:80]
	}
	return label
}

// bearerToken is the session token of the request, if any.
func bearerToken(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

// APIListSessions lists the caller's live sessions.
func (s *State) APIListSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	list, err := users.ListSessions(r.Context(), s.Pool, u.ID, bearerToken(r))
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"sessions": list})
}

// APIRevokeSession ends one of the caller's sessions.
func (s *State) APIRevokeSession(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	switch err := users.RevokeSession(r.Context(), s.Pool, u.ID, id); {
	case errors.Is(err, users.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "session not found")
	case err != nil:
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
	default:
		writeJSON(w, http.StatusOK, J{"status": "signed out"})
	}
}

// APIRevokeAllSessions ends every session of the caller, the current one too.
func (s *State) APIRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	n, err := users.RevokeAllSessions(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "signed out everywhere", "count": n})
}
