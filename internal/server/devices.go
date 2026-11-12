package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/dpop"
	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Paired apps (#101). A device signs every request with its Keystore key (DPoP,
// RFC 9449) and has no bearer secret. The server keeps the public key only.
//
// A request with a DPoP header is answered by verifyDevice in resolveCaller:
// the proof must match the method and URL, be fresh, carry a jti not seen
// before, and be signed by a key that belongs to an active device.

// deviceClockSkew is how far a proof's iat may differ from the server's clock.
const deviceClockSkew = 60 * time.Second

// maxDeviceName is the longest device name kept.
const maxDeviceName = 100

// checkProof verifies a request's DPoP proof (method, URL, freshness, unused
// jti) and returns the thumbprint of the key that signed it. On failure it
// records the refusal and returns false.
func (s *State) checkProof(r *http.Request, proof string) (string, bool) {
	now := time.Now()
	if s.BaseURL == "" {
		// The proof names the URL the app calls, which is base_url plus the path.
		logging.Warnf("device request refused: base_url is not set")
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return "", false
	}
	htu := strings.TrimSuffix(s.BaseURL, "/") + r.URL.Path
	jwk, claims, err := dpop.Verify(proof, r.Method, htu, now, deviceClockSkew)
	if err != nil {
		logging.Debugf("dpop: %v", err)
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return "", false
	}
	if err := users.RecordProofJTI(r.Context(), s.Pool, claims.Jti, now); err != nil {
		if errors.Is(err, users.ErrProofReplayed) {
			s.logRefused(r, users.CodeProofReplayed, 0, "")
			return "", false
		}
		logging.Errorf("dpop jti: %v", err)
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return "", false
	}
	jkt, err := dpop.Thumbprint(jwk)
	if err != nil {
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return "", false
	}
	return jkt, true
}

// verifyDevice authenticates a request by its DPoP proof, and records the
// device on the request log.
func (s *State) verifyDevice(r *http.Request, proof string) (users.User, bool) {
	ctx := r.Context()
	now := time.Now()
	jkt, ok := s.checkProof(r, proof)
	if !ok {
		return users.User{}, false
	}
	dev, u, err := users.DeviceByKey(ctx, s.Pool, jkt, now)
	if errors.Is(err, users.ErrNotFound) {
		st, serr := users.DevicePairingState(ctx, s.Pool, jkt, now)
		if serr != nil {
			logging.Errorf("device pairing state: %v", serr)
		}
		switch st.State {
		case users.PairingPending:
			// Waiting is not a failed sign-in: no "auth failed" line, so
			// Fail2ban does not count an app that polls while the owner
			// compares codes.
			setRefusal(r, users.CodePairingPending)
			logging.Infof("pairing pending ip=%s waiting_for=%s", s.clientIP(r), st.WaitingFor)
		case users.PairingRejected:
			setRefusal(r, users.CodePairingRejected)
			logging.Infof("pairing rejected ip=%s reason=%s", s.clientIP(r), st.Reason)
		case users.PairingExpired:
			s.logRefused(r, users.CodePairingExpired, 0, "")
		default:
			s.logRefused(r, users.CodeDeviceUnknown, 0, "")
		}
		return users.User{}, false
	}
	if err != nil {
		logging.Errorf("device lookup: %v", err)
		s.logRefused(r, users.CodeDeviceUnknown, 0, "")
		return users.User{}, false
	}
	if u.Status != users.StatusActive {
		s.logRefused(r, users.CodeUserDisabled, 0, "")
		return users.User{}, false
	}
	if l := reqLogFrom(ctx); l != nil {
		l.device = &dev
	}
	return u, true
}

// requireDevice admits a request authenticated by a device proof and returns
// the device. It is used for the device's own endpoints.
func (s *State) requireDevice(w http.ResponseWriter, r *http.Request) (users.Device, bool) {
	if _, ok := s.requireUser(w, r); !ok {
		return users.Device{}, false
	}
	l := reqLogFrom(r.Context())
	if l == nil || l.device == nil {
		writeCoded(w, http.StatusUnauthorized, users.CodeCredentialsRequired)
		return users.Device{}, false
	}
	return *l.device, true
}

// APIDeviceMe answers a device about itself. The app polls it while its
// pairing waits for approval: until then the proof is refused with
// pairing_pending.
func (s *State) APIDeviceMe(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.requireDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, J{"id": dev.ID, "name": dev.Name, "lastUsedAt": dev.LastUsedAt})
}

// pendingJSON is one pairing waiting for confirmation.
func pendingJSON(p users.PendingPairing) J {
	return J{
		"id": p.ID, "name": p.Name, "createdAt": p.CreatedAt, "expiresAt": p.ExpiresAt,
		"code": p.Code, "clientAddress": p.ClientIP,
		"ownerConfirmed": p.OwnerConfirmed, "deviceConfirmed": p.DeviceConfirmed,
	}
}

// APIPendingPairings lists pairings waiting for confirmation, with the code
// the owner compares against the one the app shows.
func (s *State) APIPendingPairings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	list, err := users.PendingPairings(r.Context(), s.Pool, u.ID, time.Now())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(list))
	for _, p := range list {
		out = append(out, pendingJSON(p))
	}
	writeJSON(w, http.StatusOK, J{"pending": out})
}

// confirmRequest is the answer to a pairing's verification code.
type confirmRequest struct {
	Result string `json:"result"`
}

// APIConfirmPendingPairing is the owner's answer on the account page:
// "match" when the code on the page equals the one on the phone, "reject"
// otherwise. A match after the device's own confirmation creates the device
// (201); before it, the pairing waits for the device (202). A rejection also
// invalidates the QR code.
func (s *State) APIConfirmPendingPairing(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in confirmRequest
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Result != "match" && in.Result != "reject" {
		writeAPIError(w, http.StatusBadRequest, `result must be "match" or "reject"`)
		return
	}
	dev, done, err := users.ConfirmPairingByOwner(r.Context(), s.Pool, u.ID, id, in.Result == "match", time.Now())
	if !s.writePairingError(w, err) {
		return
	}
	switch {
	case in.Result == "reject":
		logging.Infof("pairing rejected by owner id=%d", id)
		writeJSON(w, http.StatusOK, J{"status": "rejected"})
	case done:
		logging.Infof("pairing confirmed id=%d device=%d", id, dev.ID)
		writeJSON(w, http.StatusCreated, deviceJSON(dev))
	default:
		writeJSON(w, http.StatusAccepted, J{"status": "waiting", "waitingFor": "device"})
	}
}

// APIPairConfirm is the device's answer, signed with the key being paired:
// "match" when the code on the phone equals the one on the account page,
// "cancel" otherwise. The pending key's proof is checked here directly,
// because verifyDevice refuses a key that is not a device yet.
func (s *State) APIPairConfirm(w http.ResponseWriter, r *http.Request) {
	proof := r.Header.Get("DPoP")
	if proof == "" {
		writeCoded(w, http.StatusUnauthorized, users.CodeCredentialsRequired)
		return
	}
	jkt, ok := s.checkProof(r, proof)
	if !ok {
		s.writeRefusal(w, r)
		return
	}
	var in confirmRequest
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Result != "match" && in.Result != "cancel" {
		writeAPIError(w, http.StatusBadRequest, `result must be "match" or "cancel"`)
		return
	}
	dev, done, err := users.ConfirmPairingByDevice(r.Context(), s.Pool, jkt, in.Result == "match", time.Now())
	if errors.Is(err, users.ErrNotFound) && !errors.Is(err, users.ErrPairingExpired) {
		writeCoded(w, http.StatusNotFound, users.CodeDeviceUnknown)
		return
	}
	if !s.writePairingError(w, err) {
		return
	}
	switch {
	case in.Result == "cancel":
		logging.Infof("pairing rejected by device ip=%s", s.clientIP(r))
		writeJSON(w, http.StatusOK, J{"status": "cancelled"})
	case done:
		logging.Infof("pairing confirmed device=%d", dev.ID)
		writeJSON(w, http.StatusOK, J{"status": "paired", "device": deviceJSON(dev)})
	default:
		writeJSON(w, http.StatusAccepted, J{"status": "waiting", "waitingFor": "owner"})
	}
}

// writePairingError answers a failed confirmation and reports false, or
// reports true when err is nil.
func (s *State) writePairingError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, users.ErrPairingExpired):
		writeCoded(w, http.StatusGone, users.CodePairingExpired)
	case errors.Is(err, users.ErrPairingRejected):
		writeCoded(w, http.StatusConflict, users.CodePairingRejected)
	case errors.Is(err, users.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "pairing not found")
	default:
		logging.Errorf("pairing confirmation: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
	}
	return false
}

func deviceJSON(d users.Device) J {
	return J{"id": d.ID, "name": d.Name, "createdAt": d.CreatedAt, "lastUsedAt": d.LastUsedAt}
}

// APIListDevices lists the account's paired devices.
func (s *State) APIListDevices(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	list, err := users.Devices(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(list))
	for _, d := range list {
		out = append(out, deviceJSON(d))
	}
	writeJSON(w, http.StatusOK, J{"devices": out})
}

// APIRemoveDevice removes one of the account's devices. Its next request fails.
func (s *State) APIRemoveDevice(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := users.RemoveDevice(r.Context(), s.Pool, u.ID, id)
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "removed"})
}

// devicePairRequest is the body of a pairing that brings a device key.
type devicePairRequest struct {
	Token  string    `json:"token"`
	Device string    `json:"device"`
	JWK    *dpop.JWK `json:"jwk"`
}

// completeDevicePairing handles a pairing that sends the app's public key. It
// makes a pending pairing; no credential is returned. The app then shows the
// verification code (users.VerificationCode, computed on its side) and both
// sides confirm.
func (s *State) completeDevicePairing(w http.ResponseWriter, r *http.Request, in devicePairRequest) {
	name := strings.TrimSpace(in.Device)
	if name == "" || len(name) > maxDeviceName {
		writeAPIError(w, http.StatusBadRequest, "device name must be 1 to 100 characters")
		return
	}
	jkt, err := dpop.Thumbprint(*in.JWK)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "jwk must be a public P-256 key")
		return
	}
	p, err := users.CompleteDevicePairing(r.Context(), s.Pool, in.Token, name, jkt, s.clientIP(r), time.Now())
	switch {
	case errors.Is(err, users.ErrPairingExpired):
		writeCoded(w, http.StatusUnauthorized, users.CodePairingExpired)
		return
	case errors.Is(err, users.ErrPairingTokenUsed):
		writeCoded(w, http.StatusConflict, users.CodePairingTokenUsed)
		return
	case errors.Is(err, users.ErrTooManyPending):
		writeCoded(w, http.StatusTooManyRequests, users.CodePairingBusy)
		return
	case errors.Is(err, users.ErrNotFound):
		writeCoded(w, http.StatusUnauthorized, users.CodePairingInvalid)
		return
	case errors.Is(err, users.ErrDeviceKeyInUse):
		writeAPIError(w, http.StatusConflict, "this key is already paired")
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	logging.Infof("pairing pending id=%d ip=%s waiting_for=both", p.ID, p.ClientIP)
	writeJSON(w, http.StatusAccepted, J{
		"status":    "pending",
		"pendingId": p.ID,
		"expiresAt": p.ExpiresAt,
		"confirm":   true,
		"message":   "Compare the code on this device with the one on the account page, and confirm on both.",
	})
}
