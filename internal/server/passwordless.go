package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// APIRegisterPasskeyBegin starts sign-up with an invitation code and a passkey,
// without a password. It checks the code and username and returns WebAuthn
// options. Nothing is written until the passkey is verified in the finish step,
// so an abandoned or failed attempt leaves no account and does not use up the
// invitation.
func (s *State) APIRegisterPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	var in struct {
		Username string `json:"username"`
		Invite   string `json:"invite"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	_, err := users.InviteRoleFor(r.Context(), s.Pool, in.Invite, in.Username)
	switch {
	case errors.Is(err, users.ErrInviteInvalid):
		time.Sleep(200 * time.Millisecond) // slow down online guessing
		writeAPIError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, users.ErrExists):
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}

	// The passkey user handle is random, because the account does not exist yet
	// and so has no id to derive it from. It is stored with the account.
	handle := make([]byte, 16)
	if _, err := rand.Read(handle); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "random source failed")
		return
	}
	wu := waUser{u: users.User{Username: in.Username, WebAuthnHandle: handle}}
	options, session, err := s.webAuthn.BeginRegistration(wu)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := newCeremonyID()
	s.ceremonies.put(id, ceremony{
		session:  *session,
		expires:  time.Now().Add(ceremonyTTL),
		invite:   in.Invite,
		username: in.Username,
		handle:   handle,
	})
	writeJSON(w, http.StatusOK, J{"ceremony": id, "options": options})
}

// APIRegisterPasskeyFinish verifies the passkey and, only if it is valid,
// creates the account, stores the passkey and consumes the invitation. The
// response is a session, like password sign-up.
func (s *State) APIRegisterPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireWebAuthn(w) {
		return
	}
	cer, ok := s.ceremonies.take(r.URL.Query().Get("ceremony"))
	if !ok || cer.invite == "" {
		writeAPIError(w, http.StatusBadRequest, "registration expired; start again")
		return
	}
	wu := waUser{u: users.User{Username: cer.username, WebAuthnHandle: cer.handle}}
	cred, err := s.webAuthn.FinishRegistration(wu, cer.session, r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "passkey registration failed: "+err.Error())
		return
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "encoding error")
		return
	}
	u, err := users.RegisterPasskey(r.Context(), s.Pool, cer.invite, cer.username, cer.handle, string(raw))
	switch {
	case errors.Is(err, users.ErrInviteInvalid):
		writeAPIError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, users.ErrExists):
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, users.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	token, err := users.CreateSession(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, J{"status": u.Status, "token": token, "username": u.Username, "role": u.Role})
}
