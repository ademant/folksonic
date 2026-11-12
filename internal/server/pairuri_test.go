package server

import (
	"net/url"
	"strings"
	"testing"
)

// The pin rides in the QR payload only when the server serves its certificate.
func TestPairURICarriesTheCertificatePin(t *testing.T) {
	with := PairURI("https://192.168.1.10:4533", "tsp_x", "sha256/abc=")
	u, err := url.Parse(with)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("pin"); got != "sha256/abc=" {
		t.Errorf("pin %q, want sha256/abc=", got)
	}
	if got := u.Query().Get("token"); got != "tsp_x" {
		t.Errorf("token %q", got)
	}

	without := PairURI("https://music.example.org", "tsp_x", "")
	if strings.Contains(without, "pin") {
		t.Errorf("empty pin still in URI: %s", without)
	}
}
