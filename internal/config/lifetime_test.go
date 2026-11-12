package config

import (
	"strings"
	"testing"
)

// HTTPS is required everywhere by default, LAN included: a LAN install serves
// HTTPS with tls_cert (see tinysonic cert). The base_url does not change that.
func TestHTTPSRequiredByDefault(t *testing.T) {
	for _, baseURL := range []string{
		"https://cloud.example.com/folksonic",
		"http://203.0.113.5:4533",
		"http://192.168.1.10:4533",
		"http://127.0.0.1:4533",
		"http://folksonic.local:4533",
		"",
	} {
		cfg := &Config{BaseURL: baseURL}
		if !cfg.HTTPSRequired() {
			t.Errorf("%q: HTTPS not required by default", baseURL)
		}
	}
}

func TestTLSKeysMustComeAsAPair(t *testing.T) {
	if _, err := Load(writeSiteConfig(t, `music_dirs = ["music"]
base_url = "https://192.168.1.10:4533"
tls_cert = "cert.pem"
`)); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("tls_cert without tls_key: err = %v", err)
	}
}

func TestTLSWithPlainBaseURLWarns(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]
base_url = "http://192.168.1.10:4533"
tls_cert = "cert.pem"
tls_key = "key.pem"
`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings {
		found = found || strings.Contains(w, "not https")
	}
	if !found {
		t.Errorf("no warning for tls_cert with an http base_url: %v", c.Warnings)
	}
}

func TestRequireHTTPSOverridesTheDefault(t *testing.T) {
	on, off := true, false
	if !(&Config{BaseURL: "http://192.168.1.10:4533", RequireHTTPS: &on}).HTTPSRequired() {
		t.Error("explicit true ignored on a LAN address")
	}
	if (&Config{BaseURL: "https://cloud.example.com", RequireHTTPS: &off}).HTTPSRequired() {
		t.Error("explicit false ignored on a public name")
	}
}

func TestRequireHTTPSFromFile(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]
base_url = "https://cloud.example.com"
require_https = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPSRequired() {
		t.Error("require_https = false from the file was not honoured")
	}
}

func TestAPIKeyLifetimeDefaultsAndBounds(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]`))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKeyTTLDays != 365 || c.APIKeyIdleDays != 180 {
		t.Fatalf("defaults = %d / %d, want 365 / 180", c.APIKeyTTLDays, c.APIKeyIdleDays)
	}
	for _, bad := range []string{"api_key_ttl_days = 0", "api_key_ttl_days = 4000", "api_key_idle_days = 0"} {
		_, err := Load(writeSiteConfig(t, "music_dirs = [\"music\"]\n"+bad+"\n"))
		if err == nil || !strings.Contains(err.Error(), "must be between 1 and 3650") {
			t.Errorf("%s: err = %v, want a range error", bad, err)
		}
	}
}
