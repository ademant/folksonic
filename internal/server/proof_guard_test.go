package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Proofs from keys that are neither devices nor pending pairings are verified
// but not recorded, so a flood of new keys cannot fill the replay store (#111).
func TestUnknownKeysDoNotFillProofStore(t *testing.T) {
	s, _, _ := deviceFixture(t)
	const path = "/api/device/me"
	for i := 0; i < 50; i++ {
		k := newDeviceKey(t)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		p := proof(t, k, http.MethodGet, path)
		req.Header.Set("DPoP", p)
		if _, ok := s.verifyDevice(req, p); ok {
			t.Fatal("an unknown key was admitted")
		}
	}
	if n := s.proofs.Len(); n != 0 {
		t.Fatalf("%d proofs from unknown keys were recorded", n)
	}
}
