package server

import (
	"encoding/json"
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

// verifyDevice authenticates a request by its DPoP proof, and records the
// device on the request log.
func (s *State) verifyDevice(r *http.Request, proof string) (users.User, bool) {
	ctx := r.Context()
	now := time.Now()
	if s.BaseURL == "" {
		// The proof names the URL the app calls, which is base_url plus the path.
		logging.Warnf("device request refused: base_url is not set")
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return users.User{}, false
	}
	htu := strings.TrimSuffix(s.BaseURL, "/") + r.URL.Path
	jwk, claims, err := dpop.Verify(proof, r.Method, htu, now, deviceClockSkew)
	if err != nil {
		logging.Debugf("dpop: %v", err)
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return users.User{}, false
	}
	if err := users.RecordProofJTI(ctx, s.Pool, claims.Jti, now); err != nil {
		if errors.Is(err, users.ErrProofReplayed) {
			s.logRefused(r, users.CodeProofReplayed, 0, "")
			return users.User{}, false
		}
		logging.Errorf("dpop jti: %v", err)
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return users.User{}, false
	}
	jkt, err := dpop.Thumbprint(jwk)
	if err != nil {
		s.logRefused(r, users.CodeProofInvalid, 0, "")
		return users.User{}, false
	}
	dev, u, err := users.DeviceByKey(ctx, s.Pool, jkt, now)
	if errors.Is(err, users.ErrNotFound) {
		state, serr := users.DevicePairingState(ctx, s.Pool, jkt, now)
		if serr != nil {
			logging.Errorf("device pairing state: %v", serr)
		}
		switch state {
		case "pending":
			s.logRefused(r, users.CodePairingPending, 0, "")
		case "expired":
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

// pendingJSON is one pairing awaiting approval.
func pendingJSON(p users.PendingPairing) J {
	return J{"id": p.ID, "name": p.Name, "createdAt": p.CreatedAt, "expiresAt": p.ExpiresAt}
}

// APIPendingPairings lists pairings waiting for the account owner's approval.
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

// APIApprovePairing turns a pending pairing into a device.
func (s *State) APIApprovePairing(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dev, err := users.ApproveDevicePairing(r.Context(), s.Pool, u.ID, id, time.Now())
	switch {
	case errors.Is(err, users.ErrPairingExpired):
		writeAPIError(w, http.StatusGone, "this pairing has expired; scan a new code")
		return
	case errors.Is(err, users.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "pairing not found")
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusCreated, deviceJSON(dev))
}

// APIDenyPairing discards a pending pairing.
func (s *State) APIDenyPairing(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := users.DenyDevicePairing(r.Context(), s.Pool, u.ID, id)
	if errors.Is(err, users.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "pairing not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "denied"})
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

// completeDevicePairing handles a pairing that sends the app's public key. The
// pairing waits for approval on the account page; no credential is returned.
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
	jwkJSON, err := json.Marshal(in.JWK)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "jwk must be a public P-256 key")
		return
	}
	p, err := users.CompleteDevicePairing(r.Context(), s.Pool, in.Token, name, jkt, string(jwkJSON), time.Now())
	switch {
	case errors.Is(err, users.ErrPairingExpired):
		writeCoded(w, http.StatusUnauthorized, users.CodePairingExpired)
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
	writeJSON(w, http.StatusAccepted, J{
		"status":    "pending",
		"pendingId": p.ID,
		"expiresAt": p.ExpiresAt,
		"message":   "Approve this device on the account page before it expires.",
	})
}
