package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// sessionPost calls a session-only endpoint as the signed-in owner.
func sessionPost(t *testing.T, s *State, path, sess, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+sess)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	return rec
}

// The QR code and the link come from one token: the link redeems it once, and
// a second use is refused (#129).
func TestPairQRReturnsALinkForItsToken(t *testing.T) {
	s, _, sess := deviceFixture(t)
	rec := sessionPost(t, s, "/api/pair/qr", sess, "image/svg+xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("qr status %d: %s", rec.Code, rec.Body.String())
	}
	link := rec.Header().Get("X-Pairing-Link")
	prefix := testBase + "/pair#token="
	if !strings.HasPrefix(link, prefix) || len(link) == len(prefix) {
		t.Fatalf("link %q, want %s<token>", link, prefix)
	}
	token := strings.TrimPrefix(link, prefix)
	if _, _, err := users.RedeemPairingToken(context.Background(), s.Pool, token, "phone"); err != nil {
		t.Fatalf("first use of the link's token: %v", err)
	}
	if _, _, err := users.RedeemPairingToken(context.Background(), s.Pool, token, "phone"); err == nil {
		t.Fatal("the link's token was redeemed twice")
	}
}

// pair/start gives the same token in uri and link (#129).
func TestPairStartLinkMatchesTheURI(t *testing.T) {
	s, _, sess := deviceFixture(t)
	rec := sessionPost(t, s, "/api/pair/start", sess, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("start status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
		URI   string `json:"uri"`
		Link  string `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Link != testBase+"/pair#token="+out.Token {
		t.Fatalf("link %q does not carry token %q", out.Link, out.Token)
	}
	u, err := url.Parse(out.URI)
	if err != nil || u.Query().Get("token") != out.Token {
		t.Fatalf("uri %q does not carry the same token", out.URI)
	}
}

// An expired token is refused the same way for the link and the QR code, since
// they are one token (#129).
func TestExpiredLinkIsRefusedLikeTheQR(t *testing.T) {
	s, _, sess := deviceFixture(t)
	rec := sessionPost(t, s, "/api/pair/start", sess, "")
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	if _, err := s.Pool.Exec("UPDATE pairing_tokens SET expires_at = ?", past); err != nil {
		t.Fatal(err)
	}
	body := `{"token":"` + out.Token + `","device":"phone"}`
	req := httptest.NewRequest(http.MethodPost, "/api/pair/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	got := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(got, req)
	if got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), users.CodePairingExpired) {
		t.Fatalf("expired link: status %d body %s, want 401 %s", got.Code, got.Body.String(), users.CodePairingExpired)
	}
}

// The /pair page never needs the token: it is read from the fragment in the
// browser, and the page loads its script from this server (#129).
func TestPairPageLoadsItsScriptWithoutATokenRequest(t *testing.T) {
	s, _, _ := deviceFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/pair", nil)
	rec := httptest.NewRecorder()
	requestLogger(s.routes()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("CSP %q does not restrict scripts", rec.Header().Get("Content-Security-Policy"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy %q", rec.Header().Get("Referrer-Policy"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `src="static/pair.js"`) || strings.Contains(body, "#token=") {
		t.Fatalf("page body does not match the expected shape: %s", body)
	}
}
