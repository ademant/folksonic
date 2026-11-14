package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/passwords"
)

// Parallel guesses from one address cannot all pass the check before the first
// failure is counted: allow spends a token before any hashing (#109).
func TestParallelGuessesSpendBudgetBeforeHashing(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &authLimiter{clock: func() time.Time { return now }}
	var passed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.allow("203.0.113.5"); ok {
				passed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := passed.Load(); got != int32(authBurst) {
		t.Fatalf("%d parallel attempts passed, want exactly %d", got, int(authBurst))
	}
}

// A successful sign-in gives its token back, so correct use is not throttled.
func TestSuccessRefundsItsToken(t *testing.T) {
	l := &authLimiter{}
	for i := 0; i < int(authBurst); i++ {
		if ok, _ := l.allow("203.0.113.6"); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	l.succeed("203.0.113.6")
	if ok, _ := l.allow("203.0.113.6"); !ok {
		t.Fatal("after a success the next attempt is refused")
	}
}

// When every hashing slot is taken, sign-in answers 429 server_busy at once,
// records no failure and refunds the token, so the client is not penalised
// for the server's load.
func TestBusyHashingGateAnswers429AndRefunds(t *testing.T) {
	s := newTestState(t)
	const ip = "203.0.113.7"
	for i := 0; i < passwords.Slots; i++ {
		if !passwords.TryAcquire() {
			t.Fatal("could not take a slot")
		}
	}
	defer func() {
		for i := 0; i < passwords.Slots; i++ {
			passwords.Release()
		}
	}()

	for i := 0; i < int(authBurst)*3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		requestLogger(http.HandlerFunc(s.APILogin)).ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "server_busy") {
			t.Fatalf("attempt %d: status %d body %q, want 429 server_busy", i+1, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("busy answer has no Retry-After")
		}
	}
	if ok, _ := s.auth.allow(ip); !ok {
		t.Fatal("busy refusals used up the client's budget")
	}
}

// Passwords longer than the cap are refused before hashing (#109).
func TestPasswordLengthCap(t *testing.T) {
	long := strings.Repeat("a", passwords.MaxBytes+1)
	if _, err := passwords.Hash(long); err == nil {
		t.Fatal("Hash accepted a password over the cap")
	}
	if passwords.Verify("$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", long) {
		t.Fatal("Verify accepted a password over the cap")
	}
}
