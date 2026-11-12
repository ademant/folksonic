package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/dpop"
	"github.com/tsirysndr/tinysonic/internal/users"
)

const testBase = "https://music.test"

// deviceFixture is bob with a session and a base URL, so proofs can name it.
func deviceFixture(t *testing.T) (*State, int64, string) {
	t.Helper()
	s, _, _ := workerState(t)
	s.BaseURL = testBase
	bob := workerUserID(t, s, "bob")
	sess, err := users.CreateSession(context.Background(), s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	return s, bob, sess
}

func jwkOf(k *ecdsa.PrivateKey) map[string]string {
	pad := func(b []byte) string {
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	return map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": pad(k.X.Bytes()), "y": pad(k.Y.Bytes()),
	}
}

func newDeviceKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var jtiSeq int

// proof signs a fresh proof for method and path with the device key.
func proof(t *testing.T, k *ecdsa.PrivateKey, method, path string) string {
	t.Helper()
	jtiSeq++
	p, err := dpop.NewProof(k, method, testBase+path, fmt.Sprintf("jti-%d", jtiSeq), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func devCall(t *testing.T, s *State, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	return rec
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func withProof(p string) map[string]string { return map[string]string{"DPoP": p} }

func TestDeviceIsPairedApprovedAndRemoved(t *testing.T) {
	s, bob, sess := deviceFixture(t)
	ctx := context.Background()
	key := newDeviceKey(t)

	tok, err := users.CreatePairingToken(ctx, s.Pool, bob)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"token": tok, "device": "Pixel", "jwk": jwkOf(key)})
	rec := devCall(t, s, http.MethodPost, "/api/pair/complete", string(body), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pair: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "apiKey") || strings.Contains(rec.Body.String(), "tsk_") {
		t.Fatal("device pairing returned a bearer key")
	}
	var pending struct {
		PendingID int64 `json:"pendingId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)

	// Waiting: the device's proof is refused until the owner approves.
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, key, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodePairingPending) {
		t.Fatalf("before approval: %d %s", rec.Code, rec.Body.String())
	}

	// The account page lists it, and the owner approves it.
	rec = devCall(t, s, http.MethodGet, "/api/pair/pending", "", bearer(sess))
	if !strings.Contains(rec.Body.String(), "Pixel") {
		t.Fatalf("pending list: %s", rec.Body.String())
	}
	rec = devCall(t, s, http.MethodPost, fmt.Sprintf("/api/pair/pending/%d/approve", pending.PendingID), "", bearer(sess))
	if rec.Code != http.StatusCreated {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}

	// Approved: the device's own proof works on /api and on the Subsonic API.
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, key, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pixel") {
		t.Fatalf("device me after approval: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodGet, "/rest/getPlaylists.view", "", withProof(proof(t, key, http.MethodGet, "/rest/getPlaylists.view")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("subsonic with device proof: %d %s", rec.Code, rec.Body.String())
	}

	// A device may not manage the account: approvals, devices and keys need a session.
	rec = devCall(t, s, http.MethodGet, "/api/devices", "", withProof(proof(t, key, http.MethodGet, "/api/devices")))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), users.CodeSessionRequired) {
		t.Fatalf("device listed devices: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodGet, "/api/keys", "", withProof(proof(t, key, http.MethodGet, "/api/keys")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("device listed keys: %d %s", rec.Code, rec.Body.String())
	}

	// A captured proof cannot be replayed.
	p := proof(t, key, http.MethodGet, "/api/device/me")
	if rec := devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(p)); rec.Code != http.StatusOK {
		t.Fatalf("first use: %d", rec.Code)
	}
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(p))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeProofReplayed) {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}

	// A proof for another URL is refused.
	wrongPath := proof(t, key, http.MethodGet, "/api/keys")
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(wrongPath))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeProofInvalid) {
		t.Fatalf("proof for another URL: %d %s", rec.Code, rec.Body.String())
	}

	// The owner removes the device; its next request fails.
	var list struct {
		Devices []struct {
			ID int64 `json:"id"`
		} `json:"devices"`
	}
	rec = devCall(t, s, http.MethodGet, "/api/devices", "", bearer(sess))
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Devices) != 1 {
		t.Fatalf("devices: %s", rec.Body.String())
	}
	rec = devCall(t, s, http.MethodDelete, fmt.Sprintf("/api/devices/%d", list.Devices[0].ID), "", bearer(sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, key, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeDeviceUnknown) {
		t.Fatalf("after removal: %d %s", rec.Code, rec.Body.String())
	}
}

func TestForgedAndUnknownProofsAreRefused(t *testing.T) {
	s, _, _ := deviceFixture(t)

	// A proof signed by a key that was never paired is an unknown device.
	stranger := newDeviceKey(t)
	rec := devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, stranger, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeDeviceUnknown) {
		t.Fatalf("stranger key: %d %s", rec.Code, rec.Body.String())
	}

	// A tampered proof fails the signature check.
	p := proof(t, stranger, http.MethodGet, "/api/device/me")
	parts := strings.Split(p, ".")
	parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(strings.Join(parts, ".")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeProofInvalid) {
		t.Fatalf("forged signature: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPairingWithoutApprovalExpires(t *testing.T) {
	s, bob, sess := deviceFixture(t)
	ctx := context.Background()
	key := newDeviceKey(t)
	tok, _ := users.CreatePairingToken(ctx, s.Pool, bob)
	body, _ := json.Marshal(map[string]any{"token": tok, "device": "Late", "jwk": jwkOf(key)})
	if rec := devCall(t, s, http.MethodPost, "/api/pair/complete", string(body), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("pair: %d", rec.Code)
	}
	// Move the pairing's deadline into the past, as the clock would.
	if _, err := s.Pool.Exec("UPDATE device_pairings SET expires_at = '2020-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	rec := devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, key, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodePairingExpired) {
		t.Fatalf("expired pairing: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodGet, "/api/pair/pending", "", bearer(sess))
	if strings.Contains(rec.Body.String(), "Late") {
		t.Fatal("expired pairing still offered for approval")
	}
}

func TestPairingTokenIsSingleUseOverHTTP(t *testing.T) {
	s, bob, _ := deviceFixture(t)
	tok, _ := users.CreatePairingToken(context.Background(), s.Pool, bob)
	body, _ := json.Marshal(map[string]any{"token": tok, "device": "Once", "jwk": jwkOf(newDeviceKey(t))})
	if rec := devCall(t, s, http.MethodPost, "/api/pair/complete", string(body), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := devCall(t, s, http.MethodPost, "/api/pair/complete", string(body), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second use: %d %s", rec.Code, rec.Body.String())
	}
}
