package server

import (
	"net/http"
	"testing"
)

// #83: API clients send the key in the X-Api-Key header (DanceSonic does, as
// its AGENTS.md requires). A user key in that header must work on /api
// endpoints exactly like the apiKey query parameter.
func TestAPIKeyInHeaderAuthenticatesAPIEndpoints(t *testing.T) {
	s, _, userKey := keyFixture(t)
	const path = "/api/analysis?id=so-1"

	if code, _ := keyCall(t, s, userKey, true, http.MethodGet, path, ""); code != http.StatusOK {
		t.Errorf("header key: status %d, want 200", code)
	}
	if code, _ := keyCall(t, s, userKey, false, http.MethodGet, path, ""); code != http.StatusOK {
		t.Errorf("query key: status %d, want 200", code)
	}
	if code, _ := keyCall(t, s, "tsk_not-a-real-key", true, http.MethodGet, path, ""); code != http.StatusUnauthorized {
		t.Errorf("unknown header key: status %d, want 401", code)
	}
	if code, _ := keyCall(t, s, "", true, http.MethodGet, path, ""); code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", code)
	}
}

// The header form must not open a worker key to the rest of the API.
func TestWorkerKeyInHeaderStillRefusedOnAnalysis(t *testing.T) {
	s, workerKey, _ := keyFixture(t)
	if code, _ := keyCall(t, s, workerKey, true, http.MethodGet, "/api/analysis?id=so-1", ""); code != http.StatusUnauthorized {
		t.Errorf("worker key in header on /api/analysis: status %d, want 401", code)
	}
}
