package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

func getThrough(t *testing.T, h http.Handler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every route, pages and API alike, is served with framing and sniffing
// protection.
func TestSecurityHeadersOnEveryRoute(t *testing.T) {
	s := newTestState(t)
	h := securityHeaders(s.routes())
	for _, path := range []string{"/login", "/account", "/library", "/", "/robots.txt", "/rest/ping", "/static/login.css"} {
		rec := getThrough(t, h, path, "text/html")
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

var (
	inlineScript = regexp.MustCompile(`<script(\s[^>]*)?>\s*[^<\s]`)
	inlineStyle  = regexp.MustCompile(`<style[\s>]|\sstyle="`)
)

// The pages carry no inline script or style, and their policies do not allow
// it. A cross-site scripting bug then cannot run.
func TestPagesHaveNoInlineCodeAndStrictPolicy(t *testing.T) {
	s := newTestState(t)
	s.impressumOK = true
	s.impressumHTML = "<p>Anna</p>"
	h := securityHeaders(s.routes())
	for _, c := range []struct{ path, accept string }{
		{"/login", ""}, {"/account", ""}, {"/library", ""}, {"/", "text/html"}, {"/impressum", ""},
	} {
		rec := getThrough(t, h, c.path, c.accept)
		body := rec.Body.String()
		if inlineScript.MatchString(body) {
			t.Errorf("%s has an inline script", c.path)
		}
		if inlineStyle.MatchString(body) {
			t.Errorf("%s has an inline style", c.path)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s: no Content-Security-Policy", c.path)
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: policy allows inline code: %s", c.path, csp)
		}
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: policy does not forbid framing: %s", c.path, csp)
		}
	}
}

func TestStaticServesOnlyStylesAndScripts(t *testing.T) {
	s := newTestState(t)
	h := s.routes()
	for name, ctype := range map[string]string{
		"login.css": "text/css",
		"login.js":  "text/javascript",
	} {
		rec := getThrough(t, h, "/static/"+name, "")
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: status %d, type %q", name, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	for _, name := range []string{"login.html", "nope.js", "..%2Fserver.go"} {
		if rec := getThrough(t, h, "/static/"+name, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
}

// With legacy password sign-in off, u+p gets error 42 and the API key still
// works. Passwords stay out of the request entirely.
func TestLegacyPasswordAuthCanBeTurnedOff(t *testing.T) {
	s := newTestState(t)
	s.legacyPasswordOff = true
	ctx := context.Background()
	admin, err := users.ByName(ctx, s.Pool, "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := users.CreatePairingToken(ctx, s.Pool, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := users.RedeemPairingToken(ctx, s.Pool, token, "phone")
	if err != nil {
		t.Fatal(err)
	}

	rec := callForm(t, s.Ping, url.Values{"u": {"admin"}, "p": {"s3cret"}})
	if !strings.Contains(rec.Body.String(), `"code":42`) {
		t.Fatalf("u+p with legacy auth off: %s", rec.Body.String())
	}
	rec = callForm(t, s.Ping, url.Values{"u": {"admin"}, "p": {"enc:733363726574"}})
	if !strings.Contains(rec.Body.String(), `"code":42`) {
		t.Fatalf("enc: password with legacy auth off: %s", rec.Body.String())
	}
	if rec := callForm(t, s.Ping, url.Values{"apiKey": {key}}); !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("API key refused with legacy auth off: %s", rec.Body.String())
	}
}
