package server

import (
	"strings"
	"testing"
)

// getLicense is valid for a signed-in caller (#141).
func TestGetLicenseIsValid(t *testing.T) {
	s, _, key := keyFixture(t)
	rec := getWith(t, s.routes(), "/rest/getLicense.view", key, true)
	if !strings.Contains(rec.Body.String(), `"license":{"valid":true}`) {
		t.Fatalf("getLicense: %s", rec.Body.String())
	}
}

// tokenInfo names the user of the key that signed the request (#143).
func TestTokenInfoNamesTheKeysUser(t *testing.T) {
	s, _, key := keyFixture(t)
	rec := getWith(t, s.routes(), "/rest/tokenInfo.view", key, true)
	if !strings.Contains(rec.Body.String(), `"tokenInfo":{"username":"bob"}`) {
		t.Fatalf("tokenInfo: %s", rec.Body.String())
	}
}

// Without a key, or with one that is not valid, tokenInfo answers error 44.
func TestTokenInfoNeedsAValidKey(t *testing.T) {
	s, _, _ := keyFixture(t)
	for name, c := range map[string]struct {
		key      string
		inHeader bool
	}{
		"no key":      {"", true},
		"unknown key": {"not-a-real-key", true},
		"key in URL":  {"not-a-real-key", false},
	} {
		rec := getWith(t, s.routes(), "/rest/tokenInfo.view", c.key, c.inHeader)
		if !strings.Contains(rec.Body.String(), `"code":44`) {
			t.Errorf("%s: %s, want error 44", name, rec.Body.String())
		}
	}
}
