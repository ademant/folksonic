package config

import (
	"strings"
	"testing"
)

func TestStreamLimitDefaultsAndZero(t *testing.T) {
	p := writeSiteConfig(t, `music_dirs = ["music"]`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxStreams != DefaultMaxStreams || c.MaxStreamsPerUser != DefaultMaxStreamsPerUser {
		t.Fatalf("defaults %d/%d", c.MaxStreams, c.MaxStreamsPerUser)
	}

	p = writeSiteConfig(t, `music_dirs = ["music"]
max_streams = 0
max_streams_per_user = 3
`)
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxStreams != 0 || c.MaxStreamsPerUser != 3 {
		t.Fatalf("explicit values %d/%d: zero must disable the cap, not take the default", c.MaxStreams, c.MaxStreamsPerUser)
	}
}

func TestStreamLimitNegativeRejected(t *testing.T) {
	p := writeSiteConfig(t, `music_dirs = ["music"]
max_streams = -1
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "max_streams") {
		t.Fatalf("negative cap accepted: %v", err)
	}
}

func TestTrustedProxiesParseIPsAndRanges(t *testing.T) {
	p := writeSiteConfig(t, `music_dirs = ["music"]
trusted_proxies = ["127.0.0.1", "10.0.0.0/8", "::1"]
`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	p = writeSiteConfig(t, `music_dirs = ["music"]
trusted_proxies = ["proxy.example.org"]
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("hostname accepted as a trusted proxy: %v", err)
	}
}

func TestLegacyPasswordAuthDefaultAndOff(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.LegacyPasswordAuth {
		t.Fatal("legacy_password_auth should default to true")
	}
	c, err = Load(writeSiteConfig(t, `music_dirs = ["music"]
legacy_password_auth = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.LegacyPasswordAuth {
		t.Fatal("explicit false was overridden by the default")
	}
}
