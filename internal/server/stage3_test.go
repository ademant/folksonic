package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// fixedClock pins a limiter's time, so a test does not race the refill.
func fixedClock(l *authLimiter) {
	now := time.Unix(1_000_000_000, 0)
	l.clock = func() time.Time { return now }
}

// The table never grows past authMaxEntries, and a flood of new addresses
// evicts the fresh entries, not one that has spent its budget (#167).
func TestLimiterTableIsBounded(t *testing.T) {
	var l authLimiter
	fixedClock(&l)
	for range int(authBurst) {
		l.allow("198.51.100.7")
	}
	for i := range authMaxEntries + 50 {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if n := len(l.entries); n > authMaxEntries {
		t.Fatalf("%d entries, bound %d", n, authMaxEntries)
	}
	if ok, _ := l.allow("198.51.100.7"); ok {
		t.Fatal("the flood reset a spent address")
	}
}

// Addresses of one IPv6 /64 share a budget (#167).
func TestLimiterGroupsIPv6By64(t *testing.T) {
	var l authLimiter
	fixedClock(&l)
	for i := range int(authBurst) {
		if ok, _ := l.allow(fmt.Sprintf("2001:db8::%x", i+1)); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if ok, _ := l.allow("2001:db8::ffff"); ok {
		t.Fatal("another address of the same /64 got a fresh budget")
	}
	if ok, _ := l.allow("2001:db8:0:1::1"); !ok {
		t.Fatal("a different /64 was limited")
	}
	if limiterKey("192.0.2.1") != "192.0.2.1" || limiterKey("::ffff:192.0.2.1") != "::ffff:192.0.2.1" {
		t.Fatal("IPv4 keys changed")
	}
}

// The public begin endpoints share one per-address limit, and the token poll
// answers slow_down above its own (#168, #178).
func TestBeginEndpointsAreThrottled(t *testing.T) {
	s := passkeyState(t)
	s.beginLimit.burst, s.beginLimit.refill = beginBurst, beginRefill
	s.pollLimit.burst, s.pollLimit.refill = pollBurst, pollRefill
	fixedClock(&s.beginLimit)
	fixedClock(&s.pollLimit)
	h := s.routes()
	hdr := map[string]string{"Content-Type": "application/json"}
	for i := range beginBurst {
		path := "/api/passkey/login/begin"
		if i%2 == 1 {
			path = "/api/device/authorize"
		}
		if rec := send(h, http.MethodPost, path, hdr, nil); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d (%s) limited", i, path)
		}
	}
	for _, path := range []string{"/api/passkey/login/begin", "/api/device/authorize", "/api/oidc/begin"} {
		rec := send(h, http.MethodPost, path, hdr, nil)
		if path == "/api/oidc/begin" && rec.Code == http.StatusServiceUnavailable {
			continue // OIDC is not configured here; the check comes first
		}
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: %d %s, want 429 with Retry-After", path, rec.Code, rec.Body.String())
		}
		if m := decodeMap(t, rec); m["error"] != codeRateLimited {
			t.Fatalf("%s: %v", path, m)
		}
	}

	form := url.Values{"grant_type": {grantDeviceCode}, "device_code": {"nope"}}
	for i := range pollBurst {
		if m := decodeMap(t, send(h, http.MethodPost, "/api/device/token", nil, form)); m["error"] == "slow_down" {
			t.Fatalf("poll %d slowed", i)
		}
	}
	rec := send(h, http.MethodPost, "/api/device/token", nil, form)
	if m := decodeMap(t, rec); m["error"] != "slow_down" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("poll above the limit: %d %v", rec.Code, m)
	}
}

// The ceremony and flow stores refuse entries when full (#168).
func TestPendingStoresAreBounded(t *testing.T) {
	c := &ceremonyStore{m: map[string]ceremony{}}
	exp := time.Now().Add(time.Minute)
	for i := range maxCeremonies {
		if !c.put(fmt.Sprint(i), ceremony{expires: exp}) {
			t.Fatalf("ceremony %d refused", i)
		}
	}
	if c.put("one more", ceremony{expires: exp}) {
		t.Fatal("full ceremony store took another")
	}
	var ts timedStore[int]
	for i := range maxTimedEntries {
		if !ts.put(fmt.Sprint(i), i, time.Minute) {
			t.Fatalf("flow %d refused", i)
		}
	}
	if ts.put("one more", 0, time.Minute) {
		t.Fatal("full flow store took another")
	}
	// Expired entries make room again.
	ts.mu.Lock()
	for k, e := range ts.m {
		e.expires = time.Now().Add(-time.Second)
		ts.m[k] = e
	}
	ts.mu.Unlock()
	if !ts.put("after expiry", 0, time.Minute) {
		t.Fatal("expired entries were not pruned")
	}
}

// The relay refuses a key in the URL and a foreign Origin, before the
// upgrade (#182).
func TestRelayRefusesURLKeysAndForeignOrigins(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	s.BaseURL = "https://music.example"
	h := s.routes()
	if rec := send(h, http.MethodGet, "/api/relay/connect?device=phone-1&apiKey="+key, nil, nil); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "X-Api-Key") {
		t.Fatalf("key in URL: %d %s", rec.Code, rec.Body.String())
	}
	rec := send(h, http.MethodGet, "/api/relay/connect?device=phone-1", map[string]string{"X-Api-Key": key, "Origin": "https://evil.example"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d %s", rec.Code, rec.Body.String())
	}
	// The own origin gets as far as the upgrade check.
	rec = send(h, http.MethodGet, "/api/relay/connect?device=phone-1", map[string]string{"X-Api-Key": key, "Origin": "https://music.example"}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "WebSocket upgrade") {
		t.Fatalf("own origin: %d %s", rec.Code, rec.Body.String())
	}
}

// A frame with a reserved bit set is a protocol error (#182).
func TestRelayRefusesReservedBits(t *testing.T) {
	frame := maskedFrame(opBinary, []byte("hi"))
	frame[0] |= 0x40 // RSV1, as permessage-deflate would set
	ws := &wsConn{r: bufio.NewReader(bytes.NewReader(frame))}
	if _, _, err := ws.readFrame(); !errors.Is(err, errWSProtocol) {
		t.Fatalf("RSV1 frame: %v", err)
	}
}

// Pings spend from the frame budget like data (#182).
func TestRelayRateLimitCountsPings(t *testing.T) {
	s, _, key := keyFixture(t)
	relayRecord(t, s, key, "phone-1", "phone")
	srv := relayServer(t, s)
	phone, _, _ := dialRelay(t, srv, key, "phone-1")
	defer phone.c.Close()
	for range relayRateBurst * 3 {
		phone.send(t, opPing, []byte("x"))
	}
	if code := phone.expectClose(t); code != closePolicy {
		t.Fatalf("close code %d, want %d", code, closePolicy)
	}
}

// The hub refuses connections past relayMaxConns across all users (#182).
func TestRelayHubHasAGlobalCap(t *testing.T) {
	var h relayHub
	for i := range relayMaxConns {
		if _, ok := h.add(&relayConn{user: int64(i), dev: "d"}); !ok {
			t.Fatalf("connection %d refused", i)
		}
	}
	if _, ok := h.add(&relayConn{user: -1, dev: "d"}); ok {
		t.Fatal("connection past the global cap accepted")
	}
	h.remove(h.get(0, "d"))
	if _, ok := h.add(&relayConn{user: -1, dev: "d"}); !ok {
		t.Fatal("a freed slot was not reused")
	}
}

// The user code is stored hashed, and verification_uri is absolute (#188).
func TestDeviceUserCodeIsHashedAndURIAbsolute(t *testing.T) {
	s, session := grantState(t)
	rec := send(s.routes(), http.MethodPost, "/api/device/authorize", nil, nil)
	m := decodeMap(t, rec)
	if m["verification_uri"] != grantBase+"/account" {
		t.Fatalf("verification_uri %v", m["verification_uri"])
	}
	code := m["user_code"].(string)
	var stored string
	if err := s.Pool.QueryRow("SELECT user_code FROM oauth_device_codes").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, strings.ReplaceAll(code, "-", "")) {
		t.Fatalf("user code stored in plain: %q", stored)
	}
	approve(t, s, session, strings.ToLower(code)) // still found by its hash

	s.BaseURL = ""
	m = decodeMap(t, send(s.routes(), http.MethodPost, "/api/device/authorize", nil, nil))
	if u, _ := m["verification_uri"].(string); !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, "/account") {
		t.Fatalf("verification_uri without base_url: %q", u)
	}
}

// repoCountSearch takes only known table names (#188).
func TestRepoCountSearchTables(t *testing.T) {
	s := newTestState(t)
	if _, err := repoCountSearch(context.Background(), s.Pool, "users", ""); err == nil {
		t.Fatal("counted an arbitrary table")
	}
	if _, err := repoCountSearch(context.Background(), s.Pool, "songs", ""); err != nil {
		t.Fatal(err)
	}
}

// sessionCall sends a request with a Bearer token through the routes.
func sessionCall(s *State, method, path, token string) *httptest.ResponseRecorder {
	return send(s.routes(), method, path, map[string]string{"Authorization": "Bearer " + token, "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0"}, nil)
}

// The account lists its sessions and ends one or all of them; another
// account's session is not reachable (#189).
func TestSessionList(t *testing.T) {
	s, _, _ := keyFixture(t)
	ctx := context.Background()
	bob, admin := userIDOf(t, s, "bob"), userIDOf(t, s, "worker")
	tok1, _ := users.CreateSession(ctx, s.Pool, bob, "Firefox on Linux")
	tok2, _ := users.CreateSession(ctx, s.Pool, bob, "DanceSonic on Android")
	other, _ := users.CreateSession(ctx, s.Pool, admin, "")

	rec := sessionCall(s, http.MethodGet, "/api/sessions", tok1)
	var out struct {
		Sessions []users.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Sessions) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var otherID int64
	for _, si := range out.Sessions {
		if si.Current != (si.Label == "Firefox on Linux") || si.LastUsedAt == "" {
			t.Fatalf("session %+v", si)
		}
		if !si.Current {
			otherID = si.ID
		}
	}
	// Someone else's session id is not found.
	var foreign int64
	_ = s.Pool.QueryRow("SELECT rowid FROM sessions WHERE user_id = ?", admin).Scan(&foreign)
	if rec := sessionCall(s, http.MethodDelete, fmt.Sprintf("/api/sessions/%d", foreign), tok1); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign session: %d", rec.Code)
	}
	if rec := sessionCall(s, http.MethodDelete, fmt.Sprintf("/api/sessions/%d", otherID), tok1); rec.Code != http.StatusOK {
		t.Fatalf("revoke one: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := users.LookupSession(ctx, s.Pool, tok2); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked session still valid: %v", err)
	}
	if m := decodeMap(t, sessionCall(s, http.MethodDelete, "/api/sessions", tok1)); m["count"] != float64(1) {
		t.Fatalf("sign out everywhere: %v", m)
	}
	if rec := sessionCall(s, http.MethodGet, "/api/sessions", tok1); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after sign out everywhere: %d", rec.Code)
	}
	if _, err := users.LookupSession(ctx, s.Pool, other); err != nil {
		t.Fatalf("another account was signed out: %v", err)
	}
}

// An API key cannot list or end sessions (#189).
func TestSessionListNeedsASession(t *testing.T) {
	s, _, key := keyFixture(t)
	rec := send(s.routes(), http.MethodGet, "/api/sessions", map[string]string{"X-Api-Key": key}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api key: %d %s", rec.Code, rec.Body.String())
	}
}

// A session unused for a week ends, though its lifetime is not over (#189).
func TestSessionIdleTimeout(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	tok, _ := users.CreateSession(ctx, s.Pool, admin.ID, "")
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	if _, err := s.Pool.Exec("UPDATE sessions SET last_used_at = ?", old); err != nil {
		t.Fatal(err)
	}
	if _, err := users.LookupSession(ctx, s.Pool, tok); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("idle session accepted: %v", err)
	}
	if got := users.SessionProblem(ctx, s.Pool, tok); got != users.CodeSessionExpired {
		t.Fatalf("problem %q", got)
	}
	if n, err := users.PruneSessions(ctx, s.Pool, time.Now()); err != nil || n != 1 {
		t.Fatalf("pruned %d: %v", n, err)
	}
}

func TestSessionLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"": "unknown client",
		"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0":                         "Firefox on Linux",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 Version/17.5 Safari/605.1.15": "Safari on macOS",
		"Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 Chrome/131.0 Mobile Safari/537.36":           "Chrome on Android",
		"curl/8.5.0": "curl",
		"strange":    "strange",
	} {
		if got := sessionLabel(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

// An authenticator that does not verify its user is refused, at registration
// with a fixed message, and at sign-in (#199).
func TestPasskeyRequiresUserVerification(t *testing.T) {
	s := passkeyState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	tok, _ := users.CreateSession(ctx, s.Pool, admin.ID, "")
	auth := newSoftAuthenticator(t, "http://localhost:4598", "localhost")

	register := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/passkey/register/begin", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		s.APIPasskeyRegisterBegin(rec, req)
		var begin ceremonyBegin
		if err := json.Unmarshal(rec.Body.Bytes(), &begin); err != nil {
			t.Fatal(err)
		}
		if begin.Options.PublicKey.AuthenticatorSelection.UserVerification != "required" {
			t.Fatalf("registration options: %s", rec.Body.String())
		}
		req = httptest.NewRequest(http.MethodPost, "/api/passkey/register/finish?ceremony="+begin.Ceremony,
			bytes.NewReader(auth.registration(t, begin.Options.PublicKey.Challenge)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		s.APIPasskeyRegisterFinish(rec, req)
		return rec
	}
	auth.noUV = true
	if rec := register(); rec.Code != http.StatusBadRequest || decodeMap(t, rec)["message"] != "passkey registration failed" {
		t.Fatalf("registration without UV: %d %s", rec.Code, rec.Body.String())
	}
	auth.noUV = false
	if rec := register(); rec.Code != http.StatusCreated {
		t.Fatalf("registration with UV: %d %s", rec.Code, rec.Body.String())
	}

	auth.noUV = true
	req := httptest.NewRequest(http.MethodPost, "/api/passkey/login/begin", strings.NewReader(`{"username":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.APIPasskeyLoginBegin(rec, req)
	var begin ceremonyBegin
	if err := json.Unmarshal(rec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/passkey/login/finish?ceremony="+begin.Ceremony,
		bytes.NewReader(auth.assertion(t, begin.Options.PublicKey.Challenge)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.APIPasskeyLoginFinish(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("sign-in without user verification accepted")
	}
}

// ceremonyBegin is the part of a begin answer the tests read.
type ceremonyBegin struct {
	Ceremony string `json:"ceremony"`
	Options  struct {
		PublicKey struct {
			Challenge              string `json:"challenge"`
			AuthenticatorSelection struct {
				UserVerification string `json:"userVerification"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	} `json:"options"`
}

// A key cannot outlive api_key_ttl_days (#199).
func TestKeyLifetimeIsCappedAtTheServerTTL(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	tok, _ := users.CreateSession(ctx, s.Pool, admin.ID, "")
	defer func(old time.Duration) { users.KeyTTL = old }(users.KeyTTL)
	users.KeyTTL = 30 * 24 * time.Hour
	create := func(days int) int {
		req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(fmt.Sprintf(`{"label":"x","expires_days":%d}`, days)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := create(31); code != http.StatusBadRequest {
		t.Fatalf("31 days with a 30-day TTL: %d", code)
	}
	if code := create(30); code != http.StatusCreated {
		t.Fatalf("30 days: %d", code)
	}
}
