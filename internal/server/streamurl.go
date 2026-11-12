package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Signed stream URLs (#93). A signed-in client asks GET /api/stream-url for a
// URL per track. The URL carries the song id, the user id, an expiry and an
// HMAC over them, and the stream and download endpoints accept it in place of
// credentials. Verification needs no database lookup, only the signing key.
// Apps that know the tinysonicStreamUrl extension use this; Subsonic clients
// keep apiKeyAuthentication.

// streamSecretCache holds the signing key once it is loaded. It is read on
// the first signed request, so a rotation takes effect at the next restart.
type streamSecretCache struct {
	mu     sync.Mutex
	secret []byte
}

// streamSecret returns the signing key, loading it from the database on first
// use.
func (s *State) streamSecret(ctx context.Context) ([]byte, error) {
	s.stream.mu.Lock()
	defer s.stream.mu.Unlock()
	if s.stream.secret != nil {
		return s.stream.secret, nil
	}
	secret, err := users.LoadStreamSecret(ctx, s.Pool)
	if err != nil {
		return nil, err
	}
	s.stream.secret = secret
	return secret, nil
}

// forgetStreamSecret drops the cached key so the next use reloads it. A
// restart does the same after `tinysonic stream rotate`.
func (s *State) forgetStreamSecret() {
	s.stream.mu.Lock()
	defer s.stream.mu.Unlock()
	s.stream.secret = nil
}

// isStreamPath reports whether a signed URL may be used on this path: only the
// stream and download endpoints, which take the song id it is bound to.
func isStreamPath(p string) bool {
	switch p {
	case "/rest/stream", "/rest/stream.view", "/rest/download", "/rest/download.view":
		return true
	}
	return false
}

// verifySignedStream admits a request carrying a signed stream URL. The
// signature must match the song id, user id and expiry in the request. An
// expired link is reported as expired only after its signature checks out, so
// a forged one learns nothing about the expiry.
func (s *State) verifySignedStream(r *http.Request, q url.Values) (users.User, bool) {
	if !isStreamPath(r.URL.Path) {
		s.logRefused(r, users.CodeStreamURLInvalid, 0, "")
		return users.User{}, false
	}
	uid, err1 := strconv.ParseInt(q.Get("uid"), 10, 64)
	exp, err2 := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err1 != nil || err2 != nil {
		s.logRefused(r, users.CodeStreamURLInvalid, 0, "")
		return users.User{}, false
	}
	secret, err := s.streamSecret(r.Context())
	if err != nil {
		logging.Errorf("stream secret: %v", err)
		s.logRefused(r, users.CodeStreamURLInvalid, 0, "")
		return users.User{}, false
	}
	if !users.VerifySignedStream(secret, q.Get("id"), uid, exp, q.Get("sig")) {
		s.logRefused(r, users.CodeStreamURLInvalid, 0, "")
		return users.User{}, false
	}
	if time.Now().Unix() >= exp {
		s.logRefused(r, users.CodeStreamURLExpired, 0, "")
		return users.User{}, false
	}
	return users.User{ID: uid}, true
}

// APIStreamURL returns a signed stream URL for one song, for the signed-in
// caller. Only sessions may ask (#93 decision 4): a leaked API key must not
// be able to mint links.
func (s *State) APIStreamURL(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "id is required")
		return
	}
	_, _, found, err := s.lookupSong(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "song not found")
		return
	}
	secret, err := s.streamSecret(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	exp := time.Now().Add(users.StreamURLTTL).Unix()
	q := url.Values{
		"id":  {id},
		"uid": {strconv.FormatInt(u.ID, 10)},
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {users.SignStream(secret, id, u.ID, exp)},
	}
	writeJSON(w, http.StatusOK, J{
		"url":       s.serverURL(r) + "/rest/stream.view?" + q.Encode(),
		"expiresAt": time.Unix(exp, 0).UTC().Format(time.RFC3339),
	})
}
