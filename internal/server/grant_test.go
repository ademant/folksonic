package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/dpop"
	"github.com/tsirysndr/tinysonic/internal/users"
)

const grantBase = "http://folksonic.test"

// grantState is the keyFixture with a base URL, which DPoP proofs are checked against.
func grantState(t *testing.T) (*State, string) {
	t.Helper()
	s, _, _ := keyFixture(t)
	s.BaseURL = grantBase
	session, err := users.CreateSession(context.Background(), s.Pool, userIDOf(t, s, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	return s, session
}

// devKey is the key a device signs its proofs with.
func devKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// proofFor signs a DPoP proof for one request with a fresh jti.
func proofFor(t *testing.T, key *ecdsa.PrivateKey, method, path string) string {
	t.Helper()
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	p, err := dpop.NewProof(key, method, grantBase+path, hex.EncodeToString(b[:]), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// send makes one request. headers go in as given.
func send(h http.Handler, method, path string, headers map[string]string, form url.Values) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeMap decodes a JSON answer.
func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

// startGrant starts a device grant and returns its device code and user code.
func startGrant(t *testing.T, s *State) (deviceCode, userCode string) {
	t.Helper()
	rec := send(s.routes(), http.MethodPost, "/api/device/authorize", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	return m["device_code"].(string), m["user_code"].(string)
}

// approve signs the code in as the user of session.
func approve(t *testing.T, s *State, session, userCode string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"user_code": userCode})
	req := httptest.NewRequest(http.MethodPost, "/api/device/approve", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+session)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
}

// tokenRequest asks the token endpoint for the grant with a proof from key.
func tokenRequest(t *testing.T, s *State, key *ecdsa.PrivateKey, deviceCode string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}}
	return send(s.routes(), http.MethodPost, "/api/device/token",
		map[string]string{"DPoP": proofFor(t, key, http.MethodPost, "/api/device/token")}, form)
}

// issue runs the whole grant for key and returns its tokens.
func issue(t *testing.T, s *State, session string, key *ecdsa.PrivateKey) (access, refresh string) {
	t.Helper()
	device, user := startGrant(t, s)
	approve(t, s, session, user)
	rec := tokenRequest(t, s, key, device)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["token_type"] != "DPoP" {
		t.Fatalf("token_type = %v", m["token_type"])
	}
	return m["access_token"].(string), m["refresh_token"].(string)
}

// dpopAuth is the headers of a device request with an access token.
func dpopAuth(t *testing.T, key *ecdsa.PrivateKey, access, method, path string) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "DPoP " + access, "DPoP": proofFor(t, key, method, path)}
}

// The grant from start to a working request: the approval, the token, the
// access to the library and the stream link, and that the device cannot
// manage the account.
func TestDeviceGrantEndToEnd(t *testing.T) {
	s, session := grantState(t)
	key := devKey(t)
	access, _ := issue(t, s, session, key)

	rec := send(s.routes(), http.MethodGet, "/rest/ping.view", dpopAuth(t, key, access, http.MethodGet, "/rest/ping.view"), nil)
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("ping with the device token: %s", rec.Body.String())
	}

	rec = send(s.routes(), http.MethodGet, "/api/stream-url?id=so-1", dpopAuth(t, key, access, http.MethodGet, "/api/stream-url"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream link with the device token: %d %s", rec.Code, rec.Body.String())
	}

	rec = send(s.routes(), http.MethodPost, "/api/keys", dpopAuth(t, key, access, http.MethodPost, "/api/keys"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("key creation with the device token: %d, want 403", rec.Code)
	}
	rec = send(s.routes(), http.MethodPost, "/api/device/revoke", dpopAuth(t, key, access, http.MethodPost, "/api/device/revoke"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoke with the device token: %d, want 403", rec.Code)
	}
}

// A device token needs the proof of the key it was issued to.
func TestDeviceTokenNeedsItsKey(t *testing.T) {
	s, session := grantState(t)
	access, _ := issue(t, s, session, devKey(t))
	other := devKey(t)
	rec := send(s.routes(), http.MethodGet, "/rest/ping.view", dpopAuth(t, other, access, http.MethodGet, "/rest/ping.view"), nil)
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("another key's proof was accepted: %s", rec.Body.String())
	}
	// A bearer without any proof is refused too.
	rec = send(s.routes(), http.MethodGet, "/rest/ping.view", map[string]string{"Authorization": "DPoP " + access}, nil)
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("token without a proof was accepted: %s", rec.Body.String())
	}
}

// A proof cannot be used twice: the second request with it is refused.
func TestDeviceProofIsUsedOnce(t *testing.T) {
	s, session := grantState(t)
	key := devKey(t)
	access, _ := issue(t, s, session, key)
	proof := proofFor(t, key, http.MethodGet, "/rest/ping.view")
	headers := map[string]string{"Authorization": "DPoP " + access, "DPoP": proof}
	if rec := send(s.routes(), http.MethodGet, "/rest/ping.view", headers, nil); !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("first use: %s", rec.Body.String())
	}
	if rec := send(s.routes(), http.MethodGet, "/rest/ping.view", headers, nil); strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("replayed proof accepted: %s", rec.Body.String())
	}
}

// Refresh rotates both tokens. Using the old refresh token again revokes the
// grant, and then even the new refresh token is refused.
func TestRefreshRotationAndReuse(t *testing.T) {
	s, session := grantState(t)
	key := devKey(t)
	_, refresh := issue(t, s, session, key)

	refreshWith := func(token string) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}
		return send(s.routes(), http.MethodPost, "/api/device/refresh",
			map[string]string{"DPoP": proofFor(t, key, http.MethodPost, "/api/device/refresh")}, form)
	}
	rec := refreshWith(refresh)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	newAccess, newRefresh := m["access_token"].(string), m["refresh_token"].(string)
	if newRefresh == refresh {
		t.Fatal("refresh token did not rotate")
	}
	if rec := refreshWith(refresh); rec.Code != http.StatusBadRequest || decodeMap(t, rec)["error"] != "invalid_grant" {
		t.Fatalf("reused refresh token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := refreshWith(newRefresh); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh after reuse: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := send(s.routes(), http.MethodGet, "/rest/ping.view", dpopAuth(t, key, newAccess, http.MethodGet, "/rest/ping.view"), nil); strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatal("access token of a revoked grant was accepted")
	}
}

// The token endpoint answers the RFC 8628 states, and needs a proof.
func TestTokenEndpointStates(t *testing.T) {
	s, _ := grantState(t)
	key := devKey(t)
	device, _ := startGrant(t, s)

	if rec := tokenRequest(t, s, key, device); decodeMap(t, rec)["error"] != "authorization_pending" {
		t.Fatalf("before approval: %s", rec.Body.String())
	}
	if rec := tokenRequest(t, s, key, device); decodeMap(t, rec)["error"] != "slow_down" {
		t.Fatalf("immediate second poll: %s", rec.Body.String())
	}
	if rec := tokenRequest(t, s, key, "tsd_unknown"); decodeMap(t, rec)["error"] != "invalid_grant" {
		t.Fatalf("unknown device code: %s", rec.Body.String())
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {device}}
	if rec := send(s.routes(), http.MethodPost, "/api/device/token", nil, form); decodeMap(t, rec)["error"] != "invalid_dpop_proof" {
		t.Fatalf("no proof: %s", rec.Body.String())
	}
}

// Approving needs a signed-in session and a code that is waiting.
func TestApprovalNeedsSessionAndACode(t *testing.T) {
	s, session := grantState(t)
	_, user := startGrant(t, s)
	key := devKey(t)
	// A device proof is not a session.
	body := `{"user_code":"` + user + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/device/approve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DPoP", proofFor(t, key, http.MethodPost, "/api/device/approve"))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	// An unknown device key is refused at sign-in (401); a known device would
	// get 403 session_required. Either way the code is not approved.
	if rec.Code == http.StatusOK {
		t.Fatalf("approve with a device proof was accepted: %s", rec.Body.String())
	}
	// An unknown code is not found.
	req = httptest.NewRequest(http.MethodPost, "/api/device/approve", strings.NewReader(`{"user_code":"ZZZZ-ZZZZ"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+session)
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown code: %d, want 404", rec.Code)
	}
}

// The device grant is advertised, so apps can find it.
func TestDeviceGrantExtensionAdvertised(t *testing.T) {
	s, _ := grantState(t)
	rec := send(s.routes(), http.MethodGet, "/rest/getOpenSubsonicExtensions.view", nil, nil)
	if !strings.Contains(rec.Body.String(), `"name":"tinysonicDeviceGrant"`) {
		t.Fatalf("not advertised: %s", rec.Body.String())
	}
}
