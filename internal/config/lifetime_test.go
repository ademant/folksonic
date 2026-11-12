package config

import (
	"strings"
	"testing"
)

func TestHTTPSRequiredFollowsBaseURL(t *testing.T) {
	cases := []struct {
		name, baseURL string
		want          bool
	}{
		{"public name", "https://cloud.example.com/folksonic", true},
		{"public address", "http://203.0.113.5:4533", true},
		{"private LAN", "http://192.168.1.10:4533", false},
		{"10/8", "http://10.1.2.3:4533", false},
		{"loopback", "http://127.0.0.1:4533", false},
		{"localhost", "http://localhost:4533", false},
		{"mDNS name", "http://folksonic.local:4533", false},
		{"no base_url", "", false},
	}
	for _, c := range cases {
		cfg := &Config{BaseURL: c.baseURL}
		if got := cfg.HTTPSRequired(); got != c.want {
			t.Errorf("%s (%q): HTTPSRequired = %v, want %v", c.name, c.baseURL, got, c.want)
		}
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
