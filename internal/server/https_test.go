package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// tlsStateForTest stands in for a completed TLS handshake.
var tlsStateForTest tls.ConnectionState

// gateCall sends a request through httpsGate with the given peer and headers.
func gateCall(s *State, method, path, peer string, headers map[string]string, useTLS bool) *httptest.ResponseRecorder {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = peer
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if useTLS {
		req.TLS = &tlsStateForTest
	}
	rec := httptest.NewRecorder()
	s.httpsGate(ok).ServeHTTP(rec, req)
	return rec
}

func TestHTTPSGateRefusesPlainRequestsOnEveryEndpoint(t *testing.T) {
	s := &State{httpsRequired: true, BaseURL: "https://music.example.com"}
	const public = "198.51.100.7:5555"

	rec := gateCall(s, http.MethodPost, "/api/keys", public, nil, false)
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("API: status %d, want 426", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"https_required"`) {
		t.Errorf("API body lacks the code: %s", rec.Body)
	}
	if rec.Header().Get("Upgrade") == "" {
		t.Errorf("API: no Upgrade header")
	}

	rec = gateCall(s, http.MethodGet, "/rest/ping.view", public, nil, false)
	if rec.Code != http.StatusUpgradeRequired || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Errorf("Subsonic: status %d body %s, want 426 with the envelope", rec.Code, rec.Body)
	}

	rec = gateCall(s, http.MethodGet, "/login?next=/library", public, nil, false)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("page: status %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://music.example.com/login?next=/library" {
		t.Errorf("page: Location %q", got)
	}
}

func TestHTTPSGateAcceptsTLSAndTrustedProxies(t *testing.T) {
	s := &State{httpsRequired: true, BaseURL: "https://music.example.com",
		trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}

	if rec := gateCall(s, http.MethodGet, "/api/keys", "198.51.100.7:5555", nil, true); rec.Code != http.StatusOK {
		t.Errorf("TLS request: status %d, want 200", rec.Code)
	}
	fwd := map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}
	if rec := gateCall(s, http.MethodGet, "/api/keys", "10.0.0.5:4000", fwd, false); rec.Code != http.StatusOK {
		t.Errorf("trusted proxy with https: status %d, want 200", rec.Code)
	}
	// The same header from an address that is not a trusted proxy is ignored.
	if rec := gateCall(s, http.MethodGet, "/api/keys", "198.51.100.7:5555", fwd, false); rec.Code != http.StatusUpgradeRequired {
		t.Errorf("untrusted peer claiming https: status %d, want 426", rec.Code)
	}
	// A trusted proxy that says http is refused.
	plain := map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "http"}
	if rec := gateCall(s, http.MethodGet, "/api/keys", "10.0.0.5:4000", plain, false); rec.Code != http.StatusUpgradeRequired {
		t.Errorf("trusted proxy with http: status %d, want 426", rec.Code)
	}
}

func TestHTTPSGateAllowsLocalToolsOnly(t *testing.T) {
	s := &State{httpsRequired: true, BaseURL: "https://music.example.com"}
	// The deploy health check: loopback, no proxy headers.
	if rec := gateCall(s, http.MethodGet, "/rest/ping.view", "127.0.0.1:50000", nil, false); rec.Code != http.StatusOK {
		t.Errorf("local tool: status %d, want 200", rec.Code)
	}
	// A request a proxy passed on is not local, even from loopback.
	proxied := map[string]string{"X-Forwarded-For": "198.51.100.7"}
	if rec := gateCall(s, http.MethodGet, "/rest/ping.view", "127.0.0.1:50000", proxied, false); rec.Code != http.StatusUpgradeRequired {
		t.Errorf("loopback with X-Forwarded-For: status %d, want 426", rec.Code)
	}
}

func TestHTTPSGateOffAllowsPlainHTTP(t *testing.T) {
	s := &State{httpsRequired: false, BaseURL: "http://192.168.1.10:4533"}
	if rec := gateCall(s, http.MethodGet, "/api/keys", "192.168.1.20:5555", nil, false); rec.Code != http.StatusOK {
		t.Errorf("enforcement off: status %d, want 200", rec.Code)
	}
}

// Refusals carry their code on the JSON API and a readable message on Subsonic.
func TestRefusedCredentialsCarryTheirCode(t *testing.T) {
	s, workerKey, userKey := keyFixture(t)
	h := requestLogger(s.routes())
	call := func(key string, inHeader bool, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(body))
		if key != "" && inHeader {
			req.Header.Set("X-Api-Key", key)
		}
		if key != "" && !inHeader {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			req.URL.RawQuery = strings.TrimPrefix(sep+"apiKey="+key, "?")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, decodeJSON(t, rec.Body.String())
	}

	if code, body := call("", false, "/api/keys", ""); code != http.StatusUnauthorized || body["error"] != "credentials_required" {
		t.Errorf("no credentials: %d %v", code, body)
	}
	if code, body := call(workerKey, true, "/api/keys", ""); code != http.StatusUnauthorized || body["error"] != "scope_forbidden" {
		t.Errorf("worker key on /api/keys: %d %v", code, body)
	}

	expireKey(t, s, userKey)
	if code, body := call(userKey, true, "/api/keys", ""); code != http.StatusUnauthorized || body["error"] != "key_expired" || body["message"] == "" {
		t.Errorf("expired key: %d %v", code, body)
	}
	// On Subsonic the same refusal is error 40 with a message that says why.
	_, body := call(userKey, false, "/rest/ping.view", "")
	env := body["subsonic-response"].(map[string]any)
	errObj := env["error"].(map[string]any)
	if env["status"] != "failed" || errObj["code"] != float64(40) || !strings.Contains(errObj["message"].(string), "expired") {
		t.Errorf("Subsonic expired key: %v", env)
	}
}

func TestExpiredPairingTokenAnswersItsCode(t *testing.T) {
	s, _, _ := keyFixture(t)
	tok := mustPairingToken(t, s)
	expirePairing(t, s)
	h := requestLogger(s.routes())
	req := httptest.NewRequest(http.MethodPost, "/api/pair/complete", strings.NewReader(`{"token":"`+tok+`","device":"phone"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := decodeJSON(t, rec.Body.String())
	if rec.Code != http.StatusUnauthorized || body["error"] != "pairing_expired" {
		t.Fatalf("expired pairing: %d %v", rec.Code, body)
	}
}

func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}
	return m
}

// expireKey sets the expiry of the test key labelled "phone" in the past.
func expireKey(t *testing.T, s *State, _ string) {
	t.Helper()
	if _, err := s.Pool.Exec("UPDATE api_keys SET expires_at = '2020-01-01T00:00:00Z' WHERE label = 'phone'"); err != nil {
		t.Fatal(err)
	}
}

func mustPairingToken(t *testing.T, s *State) string {
	t.Helper()
	tok, err := users.CreatePairingToken(context.Background(), s.Pool, userIDOf(t, s, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func expirePairing(t *testing.T, s *State) {
	t.Helper()
	if _, err := s.Pool.Exec("UPDATE pairing_tokens SET expires_at = '2020-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
}
