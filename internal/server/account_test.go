package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// bearerCall sends an authenticated request through the real router.
func bearerCall(t *testing.T, s *State, token, method, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// A passkey can be removed while a password remains, but not when it is the
// last way to sign in.
func TestPasskeyRemovalKeepsOneSignInMethod(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	withPassword, err := users.Create(ctx, s.Pool, "ivan", "pw", users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.AddPasskey(ctx, s.Pool, withPassword.ID, "{}", "laptop"); err != nil {
		t.Fatal(err)
	}
	token, _ := users.CreateSession(ctx, s.Pool, withPassword.ID)
	list := bearerCall(t, s, token, http.MethodGet, "/api/passkeys", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"label":"laptop"`) {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	recs, _ := users.ListPasskeys(ctx, s.Pool, withPassword.ID)
	if rec := bearerCall(t, s, token, http.MethodDelete, "/api/passkeys/"+itoa(recs[0].ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("remove with password present: status %d", rec.Code)
	}

	// An account with no password and no identity keeps its only passkey.
	judy := passwordlessAccount(t, s, "judy")
	if err := users.AddPasskey(ctx, s.Pool, judy.ID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	judyToken, _ := users.CreateSession(ctx, s.Pool, judy.ID)
	passkeys, _ := users.ListPasskeys(ctx, s.Pool, judy.ID)
	rec := bearerCall(t, s, judyToken, http.MethodDelete, "/api/passkeys/"+itoa(passkeys[0].ID), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("removing the last sign-in method: status %d, want 409", rec.Code)
	}
}

// passwordlessAccount creates an account with no password and no identity, as
// a passkey-only sign-up does, by registering through an identity and then
// unlinking it.
func passwordlessAccount(t *testing.T, s *State, name string) users.User {
	t.Helper()
	ctx := context.Background()
	code, _, err := users.CreateInvite(ctx, s.Pool, users.RoleUser, name, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := users.RegisterOIDC(ctx, s.Pool, code, name, "https://idp.example.test", "subject-"+name)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := users.ListOIDCIdentities(ctx, s.Pool, acc.ID)
	if err := users.RemoveOIDCIdentity(ctx, s.Pool, acc.ID, ids[0].ID); err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestPasskeyCannotBeRemovedByAnotherAccount(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	owner, _ := users.Create(ctx, s.Pool, "kim", "pw", users.RoleUser, users.StatusActive)
	if err := users.AddPasskey(ctx, s.Pool, owner.ID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	recs, _ := users.ListPasskeys(ctx, s.Pool, owner.ID)
	intruder, _ := users.Create(ctx, s.Pool, "lee", "pw", users.RoleUser, users.StatusActive)
	token, _ := users.CreateSession(ctx, s.Pool, intruder.ID)
	if rec := bearerCall(t, s, token, http.MethodDelete, "/api/passkeys/"+itoa(recs[0].ID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if left, _ := users.ListPasskeys(ctx, s.Pool, owner.ID); len(left) != 1 {
		t.Fatal("another account removed the passkey")
	}
}

// The QR endpoint returns SVG by default and PNG when asked for it. Each call
// issues a fresh single-use pairing token.
func TestPairQRFormats(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	mona, _ := users.Create(ctx, s.Pool, "mona", "pw", users.RoleUser, users.StatusActive)
	token, _ := users.CreateSession(ctx, s.Pool, mona.ID)

	svg := bearerCall(t, s, token, http.MethodPost, "/api/pair/qr", "image/svg+xml")
	if svg.Code != http.StatusOK || svg.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("svg: status %d type %q", svg.Code, svg.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(svg.Body.String(), "<svg") || !strings.Contains(svg.Body.String(), `<path fill="#000"`) {
		t.Fatalf("svg body is not a QR code: %.80s", svg.Body.String())
	}
	if svg.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pairing QR may be cached")
	}

	png := bearerCall(t, s, token, http.MethodPost, "/api/pair/qr", "image/png")
	if png.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(png.Body.String(), "\x89PNG") {
		t.Fatalf("png: type %q", png.Header().Get("Content-Type"))
	}

	anon := httptest.NewRecorder()
	s.routes().ServeHTTP(anon, httptest.NewRequest(http.MethodPost, "/api/pair/qr", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous QR: status %d, want 401", anon.Code)
	}
}

func TestAccountPageServedWithoutSessionCheck(t *testing.T) {
	s := newTestState(t)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connect the Android app") {
		t.Fatalf("account page: status %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "img-src 'self' blob:") {
		t.Fatal("account page CSP does not allow the QR image")
	}
}

// The SVG must draw the same modules as the library's own image. Comparing
// against go-qrcode's rendering checks the bitmap orientation and the path
// coordinates without a QR decoder.
func TestQRSVGMatchesRenderedImage(t *testing.T) {
	code, err := qrcode.New(PairURI("https://music.example.org", "tsp_example"), qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bitmap := code.Bitmap()
	var svg bytes.Buffer
	if err := writeQRSVG(&svg, bitmap); err != nil {
		t.Fatal(err)
	}
	dark := map[[2]int]bool{}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h1v1h-1z`).FindAllStringSubmatch(svg.String(), -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		dark[[2]int{x, y}] = true
	}
	img := code.Image(-1) // one pixel per module, quiet zone included
	b := img.Bounds()
	if b.Dx() != len(bitmap) || b.Dy() != len(bitmap) {
		t.Fatalf("image %dx%d, bitmap %d", b.Dx(), b.Dy(), len(bitmap))
	}
	for y := range len(bitmap) {
		for x := range len(bitmap) {
			r, _, _, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			isDark := r < 0x8000
			if isDark != dark[[2]int{x, y}] {
				t.Fatalf("module (%d,%d): image dark=%v, svg dark=%v", x, y, isDark, dark[[2]int{x, y}])
			}
		}
	}
}
