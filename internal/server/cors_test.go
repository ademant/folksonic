package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cors runs one request through the CORS middleware and the routes.
func cors(t *testing.T, s *State, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	corsMiddleware(s.routes()).ServeHTTP(rec, req)
	return rec
}

// #220: a preflight for PUT with the Authorization header is allowed, and the
// request headers are listed rather than a wildcard.
func TestCORSPreflightAllowsPUTAndAuthorization(t *testing.T) {
	s, _, _ := keyFixture(t)
	rec := cors(t, s, http.MethodOptions, "/api/relay/devices/phone-1", map[string]string{
		"Access-Control-Request-Method":  "PUT",
		"Access-Control-Request-Headers": "authorization, content-type",
		"Origin":                         "https://client.example",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d", rec.Code)
	}
	methods := rec.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, "PUT") {
		t.Fatalf("Allow-Methods %q has no PUT", methods)
	}
	allowed := rec.Header().Get("Access-Control-Allow-Headers")
	if allowed == "*" || !strings.Contains(allowed, "Authorization") || !strings.Contains(allowed, "X-Api-Key") {
		t.Fatalf("Allow-Headers %q must list Authorization and X-Api-Key", allowed)
	}
}

// #220: a plain OPTIONS reaches the route, so the endpoint's own Allow comes back.
func TestCORSOptionsCarriesEndpointAllow(t *testing.T) {
	s, _, _ := keyFixture(t)
	rec := cors(t, s, http.MethodOptions, "/api/device/me", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("OPTIONS on a GET route: %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow %q does not name GET", allow)
	}
}

// Subsonic paths keep their 204, with the methods their handlers accept, also
// behind a base path.
func TestCORSSubsonicOptionsUnchanged(t *testing.T) {
	s, _, _ := keyFixture(t)
	for _, path := range []string{"/rest/ping.view", "/folksonic/rest/ping.view"} {
		rec := cors(t, s, http.MethodOptions, path, nil)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: %d, want 204", path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD, POST, OPTIONS" {
			t.Errorf("%s: Allow %q", path, allow)
		}
	}
}

// Browser JavaScript can read the pairing link and the retry delay.
func TestCORSExposesPairingAndRetryHeaders(t *testing.T) {
	s, _, _ := keyFixture(t)
	rec := cors(t, s, http.MethodGet, "/rest/ping.view", nil)
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, h := range []string{"X-Pairing-Link", "Retry-After"} {
		if !strings.Contains(exposed, h) {
			t.Errorf("Expose-Headers %q does not include %s", exposed, h)
		}
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("Allow-Origin is not *")
	}
}
