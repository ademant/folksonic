package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

func TestAuthLimiterBurstThenRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &authLimiter{clock: func() time.Time { return now }}
	for i := 0; i < int(authBurst); i++ {
		if ok, _ := l.allow("198.51.100.7"); !ok {
			t.Fatalf("attempt %d refused within the burst", i+1)
		}
		l.fail("198.51.100.7")
	}
	ok, wait := l.allow("198.51.100.7")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("after the burst: allowed=%v wait=%v, want refused with a short wait", ok, wait)
	}
	// Another address is not affected.
	if ok, _ := l.allow("198.51.100.8"); !ok {
		t.Fatal("a different address was refused")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow("198.51.100.7"); !ok {
		t.Fatal("one second later a single attempt is not allowed")
	}
}

// Successful attempts cost nothing, so a client that signs in correctly is
// never throttled by its own traffic.
func TestAuthLimiterSuccessesDoNotSpendBudget(t *testing.T) {
	l := &authLimiter{}
	for i := 0; i < 100; i++ {
		if ok, _ := l.allow("198.51.100.9"); !ok {
			t.Fatalf("request %d refused without any failure", i)
		}
		l.succeed("198.51.100.9")
	}
}

func TestProgressiveDelayGrowsAndIsCapped(t *testing.T) {
	want := map[int]time.Duration{
		0:  0,
		1:  10 * time.Millisecond,
		2:  20 * time.Millisecond,
		3:  40 * time.Millisecond,
		4:  80 * time.Millisecond,
		5:  100 * time.Millisecond,
		50: 100 * time.Millisecond,
	}
	for n, d := range want {
		if got := delayFor(n); got != d {
			t.Errorf("delayFor(%d) = %v, want %v", n, got, d)
		}
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	l := &authLimiter{}
	l.fail("198.51.100.10")
	l.fail("198.51.100.10")
	if d := l.fail("198.51.100.10"); d != 40*time.Millisecond {
		t.Fatalf("third failure delay %v", d)
	}
	l.succeed("198.51.100.10")
	if d := l.fail("198.51.100.10"); d != 10*time.Millisecond {
		t.Fatalf("delay after a success %v, want the base delay", d)
	}
}

// After the failure budget is spent, password guesses are refused with 429
// before any hashing, while an API key from the same address still works.
func TestPasswordGuessesThrottledButAPIKeyUnaffected(t *testing.T) {
	s := newTestState(t)
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

	// Through requestLogger, as the server runs handlers, so a throttled
	// refusal is known to the response writer.
	ping := requestLogger(http.HandlerFunc(s.Ping))
	call := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/rest/ping", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		ping.ServeHTTP(rec, req)
		return rec
	}

	bad := url.Values{"u": {"admin"}, "p": {"wrong"}}
	for i := 0; i < int(authBurst); i++ {
		rec := call(bad)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":40`) {
			t.Fatalf("failure %d: status %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := call(bad)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after the budget: status %d, want 429", rec.Code)
	}
	if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra < 1 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	// The right password is refused too while throttled: the limit is per address.
	if rec := call(url.Values{"u": {"admin"}, "p": {"s3cret"}}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password while throttled: status %d, want 429", rec.Code)
	}
	if rec := call(url.Values{"apiKey": {key}}); !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("API key refused during the throttle: %s", rec.Body.String())
	}
}

// A failure is logged with its address and reason. The username and the
// password never appear in the log, including for unknown usernames.
func TestAuthFailuresLogWithoutCredentials(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(&buf, slog.LevelInfo, false)))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := newTestState(t)
	callForm(t, s.Ping, url.Values{"u": {"nobody-here"}, "p": {"hunter2"}})
	callForm(t, s.Ping, url.Values{"u": {"admin"}, "p": {"hunter3"}})
	out := buf.String()

	for _, want := range []string{
		"auth failed ip=192.0.2.1 reason=unknown-user path=/rest/x",
		"auth failed ip=192.0.2.1 reason=password path=/rest/x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"nobody-here", "hunter2", "hunter3", "admin"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
}

func TestClientIPTrustsForwardedHeaderOnlyFromProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"direct client ignores header", "203.0.113.5:4000", "198.51.100.1", "203.0.113.5"},
		{"trusted proxy: client from header", "10.0.0.2:4000", "198.51.100.1", "198.51.100.1"},
		{"rightmost untrusted hop wins", "10.0.0.2:4000", "198.51.100.1, 10.0.0.9", "198.51.100.1"},
		{"malformed rightmost hop stops the search", "10.0.0.2:4000", "198.51.100.1, not-an-ip", "10.0.0.2"},
		{"no header from proxy", "10.0.0.2:4000", "", "10.0.0.2"},
		{"ipv6 proxy", "[::1]:4000", "2001:db8::7", "2001:db8::7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/rest/ping", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r, trusted); got != c.want {
				t.Errorf("clientIP = %s, want %s", got, c.want)
			}
		})
	}
}

func TestStreamSlotsAreCappedAndReleased(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, err := users.ByName(ctx, s.Pool, "admin")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "track.flac")
	if err := os.WriteFile(file, []byte("audio-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')",
		"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-1', '" +
			file + "', 'x', 'A', 'ar-1', 'B', 'al-1', 'flac', 'audio/flac', 1)",
	} {
		if _, err := s.Pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.maxStreams, s.maxStreamsPerUser = 1, 0
	auth := url.Values{"u": {"admin"}, "p": {"s3cret"}, "id": {"so-1"}}

	// The only slot is taken: the next stream is refused, not queued.
	release, ok := s.streams.acquire(admin.ID, s.maxStreams, s.maxStreamsPerUser)
	if !ok {
		t.Fatal("could not take the slot")
	}
	rec := callForm(t, s.Stream, auth)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("stream over the cap: status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("overload answer is not a Subsonic envelope: %s", rec.Body.String())
	}

	// Freed slot: the stream works, and it releases its slot when done.
	release()
	release() // a second call must not free a slot twice
	if rec := callForm(t, s.Stream, auth); rec.Code != http.StatusOK || rec.Body.String() != "audio-bytes" {
		t.Fatalf("stream after release: status %d, body %q", rec.Code, rec.Body.String())
	}
	if rel, ok := s.streams.acquire(admin.ID, 1, 0); !ok {
		t.Fatal("slot not released when the stream ended")
	} else {
		rel()
	}
}

func TestStreamPerUserCap(t *testing.T) {
	var l streamLimiter
	relA, ok := l.acquire(1, 0, 1)
	if !ok {
		t.Fatal("first stream refused")
	}
	if _, ok := l.acquire(1, 0, 1); ok {
		t.Fatal("second stream of one account allowed past the per-user cap")
	}
	if rel, ok := l.acquire(2, 0, 1); !ok {
		t.Fatal("another account refused by the first account's cap")
	} else {
		rel()
	}
	relA()
	if rel, ok := l.acquire(1, 0, 1); !ok {
		t.Fatal("slot not freed for the account")
	} else {
		rel()
	}
}

func TestAPILoginThrottledAfterFailures(t *testing.T) {
	s := newTestState(t)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.APILogin(rec, req)
		return rec
	}
	for i := 0; i < int(authBurst); i++ {
		if rec := post(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, rec.Code)
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("after the budget: status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
