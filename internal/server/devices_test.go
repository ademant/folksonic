package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	// Waiting: the device's proof is refused until both sides confirm. That
	// is not a failed sign-in, so it writes no "auth failed" line for Fail2ban.
	logBuf := captureLog(t)
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, key, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodePairingPending) {
		t.Fatalf("before confirmation: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logBuf.String(), "auth failed") || !strings.Contains(logBuf.String(), "pairing pending") {
		t.Fatalf("pending logged as: %s", logBuf.String())
	}

	// The account page lists it with the code the app computes on its side.
	jkt, _ := dpop.Thumbprint(dpop.JWK{Kty: "EC", Crv: "P-256", X: jwkOf(key)["x"], Y: jwkOf(key)["y"]})
	wantCode := users.VerificationCode(sha256Hex(tok), jkt)
	rec = devCall(t, s, http.MethodGet, "/api/pair/pending", "", bearer(sess))
	if !strings.Contains(rec.Body.String(), "Pixel") || !strings.Contains(rec.Body.String(), `"code":"`+wantCode+`"`) {
		t.Fatalf("pending list (want code %s): %s", wantCode, rec.Body.String())
	}
	confirmURL := fmt.Sprintf("/api/pair/pending/%d/confirm", pending.PendingID)

	// The owner's confirmation alone creates nothing.
	rec = devCall(t, s, http.MethodPost, confirmURL, `{"result":"match"}`, bearer(sess))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"waitingFor":"device"`) {
		t.Fatalf("owner confirm: %d %s", rec.Code, rec.Body.String())
	}
	// The device confirms with a request signed by the key being paired.
	rec = devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, key, http.MethodPost, "/api/pair/confirm")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"paired"`) {
		t.Fatalf("device confirm: %d %s", rec.Code, rec.Body.String())
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

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pairBody is a pairing request for key.
func pairBody(tok, name string, key *ecdsa.PrivateKey) string {
	b, _ := json.Marshal(map[string]any{"token": tok, "device": name, "jwk": jwkOf(key)})
	return string(b)
}

func TestSecondDeviceCannotUseAConfirmedCode(t *testing.T) {
	s, bob, sess := deviceFixture(t)
	tok, _ := users.CreatePairingToken(context.Background(), s.Pool, bob)
	mine, other := newDeviceKey(t), newDeviceKey(t)

	rec := devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Mine", mine), nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"confirm":true`) {
		t.Fatalf("mine: %d %s", rec.Code, rec.Body.String())
	}
	var p struct {
		PendingID int64 `json:"pendingId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec := devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Mine", other), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("other device scanning the same code: %d %s", rec.Code, rec.Body.String())
	}

	// Both sides confirm "mine".
	if rec := devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, mine, http.MethodPost, "/api/pair/confirm"))); rec.Code != http.StatusAccepted {
		t.Fatalf("device confirm: %d %s", rec.Code, rec.Body.String())
	}
	if rec := devCall(t, s, http.MethodPost, fmt.Sprintf("/api/pair/pending/%d/confirm", p.PendingID), `{"result":"match"}`, bearer(sess)); rec.Code != http.StatusCreated {
		t.Fatalf("owner confirm: %d %s", rec.Code, rec.Body.String())
	}

	// The other device is out: its pairing is rejected and the code is used up.
	rec = devCall(t, s, http.MethodGet, "/api/device/me", "", withProof(proof(t, other, http.MethodGet, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodePairingRejected) {
		t.Fatalf("other device after consumption: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, other, http.MethodPost, "/api/pair/confirm")))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), users.CodePairingRejected) {
		t.Fatalf("other device confirming: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Third", newDeviceKey(t)), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), users.CodePairingTokenUsed) {
		t.Fatalf("scan after consumption: %d %s", rec.Code, rec.Body.String())
	}
	// Nor can the code be redeemed for an API key the old way.
	rec = devCall(t, s, http.MethodPost, "/api/pair/complete", fmt.Sprintf(`{"token":%q,"device":"legacy"}`, tok), nil)
	if rec.Code < 400 || strings.Contains(rec.Body.String(), "apiKey") {
		t.Fatalf("legacy redemption: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOwnerRejectsAPairing(t *testing.T) {
	s, bob, sess := deviceFixture(t)
	tok, _ := users.CreatePairingToken(context.Background(), s.Pool, bob)
	evil := newDeviceKey(t)
	rec := devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Phone", evil), nil)
	var p struct {
		PendingID int64 `json:"pendingId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	rec = devCall(t, s, http.MethodPost, fmt.Sprintf("/api/pair/pending/%d/confirm", p.PendingID), `{"result":"reject"}`, bearer(sess))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rejected") {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, evil, http.MethodPost, "/api/pair/confirm")))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), users.CodePairingRejected) {
		t.Fatalf("rejected device confirming: %d %s", rec.Code, rec.Body.String())
	}
	rec = devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Again", newDeviceKey(t)), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), users.CodePairingTokenUsed) {
		t.Fatalf("scan after rejection: %d %s", rec.Code, rec.Body.String())
	}
	// Unknown results are refused.
	if rec := devCall(t, s, http.MethodPost, fmt.Sprintf("/api/pair/pending/%d/confirm", p.PendingID), `{"result":"yes"}`, bearer(sess)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad result: %d", rec.Code)
	}
}

func TestDeviceConfirmationNeedsTheKeysProof(t *testing.T) {
	s, bob, _ := deviceFixture(t)
	tok, _ := users.CreatePairingToken(context.Background(), s.Pool, bob)
	key := newDeviceKey(t)
	if rec := devCall(t, s, http.MethodPost, "/api/pair/complete", pairBody(tok, "Phone", key), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("pair: %d", rec.Code)
	}
	// No proof at all.
	if rec := devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no proof: %d %s", rec.Code, rec.Body.String())
	}
	// A proof for another URL.
	rec := devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, key, http.MethodPost, "/api/device/me")))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), users.CodeProofInvalid) {
		t.Fatalf("wrong URL: %d %s", rec.Code, rec.Body.String())
	}
	// A key with no pairing.
	rec = devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, withProof(proof(t, newDeviceKey(t), http.MethodPost, "/api/pair/confirm")))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), users.CodeDeviceUnknown) {
		t.Fatalf("stranger: %d %s", rec.Code, rec.Body.String())
	}
	// A session cannot confirm for the device.
	sess, _ := users.CreateSession(context.Background(), s.Pool, bob)
	if rec := devCall(t, s, http.MethodPost, "/api/pair/confirm", `{"result":"match"}`, bearer(sess)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session confirming for the device: %d", rec.Code)
	}
}
