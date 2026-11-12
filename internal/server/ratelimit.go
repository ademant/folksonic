package server

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// Limits on password guessing and on how many audio streams are open at once.
// Both are in memory: a restart clears them, which is acceptable for guards
// against online abuse.

// authBurst is how many failed password attempts one address may make at once.
// authRefill is how many failures per second are forgiven afterwards, which
// keeps a sustained guess to about one attempt per second.
const (
	authBurst  = 5.0
	authRefill = 1.0
)

// Progressive delay after failures: authDelayBase doubles per consecutive
// failure up to authDelayCap. A success resets it.
const (
	authDelayBase = 10 * time.Millisecond
	authDelayCap  = 100 * time.Millisecond
)

// authEntry tracks one client address.
type authEntry struct {
	tokens float64
	last   time.Time
	fails  int
}

// authLimiter throttles failed password attempts per client address. Only
// failures spend the budget. Subsonic clients send their credentials with every
// request, including cover art, so counting successes would throttle normal
// use. The zero value is ready to use.
type authLimiter struct {
	mu      sync.Mutex
	clock   func() time.Time // nil means time.Now; tests set it
	entries map[string]*authEntry
}

// authMaxEntries bounds memory. Entries idle for authIdle are dropped when the
// table is full.
const (
	authMaxEntries = 10000
	authIdle       = 10 * time.Minute
)

func (l *authLimiter) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// entry returns the state for ip, refilled to now. Callers hold l.mu.
func (l *authLimiter) entry(ip string, now time.Time) *authEntry {
	if l.entries == nil {
		l.entries = map[string]*authEntry{}
	}
	if len(l.entries) >= authMaxEntries {
		for k, e := range l.entries {
			if now.Sub(e.last) > authIdle {
				delete(l.entries, k)
			}
		}
	}
	e, ok := l.entries[ip]
	if !ok {
		e = &authEntry{tokens: authBurst, last: now}
		l.entries[ip] = e
	}
	e.tokens = min(authBurst, e.tokens+now.Sub(e.last).Seconds()*authRefill)
	e.last = now
	return e
}

// allow reports whether ip may try a password now. When it may not, retryAfter
// says how long until the next attempt is allowed. Allowed attempts cost
// nothing; fail spends the budget.
func (l *authLimiter) allow(ip string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip, l.now())
	if e.tokens >= 1 {
		return true, 0
	}
	wait := (1 - e.tokens) / authRefill
	return false, time.Duration(wait * float64(time.Second))
}

// fail records a failed attempt from ip. It spends a token and returns how
// long the caller should wait before answering.
func (l *authLimiter) fail(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip, l.now())
	if e.tokens >= 1 {
		e.tokens--
	}
	e.fails++
	return delayFor(e.fails)
}

// succeed clears the consecutive-failure count for ip.
func (l *authLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[ip]; ok {
		e.fails = 0
	}
}

// delayFor returns the wait after the n-th consecutive failure.
func delayFor(n int) time.Duration {
	if n < 1 {
		return 0
	}
	d := authDelayBase << min(n-1, 4)
	return min(d, authDelayCap)
}

// streamLimiter counts audio streams in progress, in total and per account.
// Zero limits mean no cap. The zero value is ready to use.
type streamLimiter struct {
	mu      sync.Mutex
	total   int
	perUser map[int64]int
}

// acquire takes a stream slot for userID. The returned release must be called
// when the stream ends; it is safe to call more than once.
func (l *streamLimiter) acquire(userID int64, maxTotal, maxPerUser int) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perUser == nil {
		l.perUser = map[int64]int{}
	}
	if maxTotal > 0 && l.total >= maxTotal {
		return nil, false
	}
	if maxPerUser > 0 && l.perUser[userID] >= maxPerUser {
		return nil, false
	}
	l.total++
	l.perUser[userID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.perUser[userID]--; l.perUser[userID] == 0 {
				delete(l.perUser, userID)
			}
		})
	}, true
}

// streamIdleTimeout bounds a stream that stops making progress. Each write
// moves the deadline, so a long but moving stream is not cut off.
const streamIdleTimeout = 60 * time.Second

// idleWriter sets a write deadline before each write to a stream. A client
// that stops reading loses the connection after streamIdleTimeout, which
// frees its stream slot.
type idleWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w idleWriter) Write(p []byte) (int, error) {
	// Not every writer supports deadlines (test recorders do not); the stream
	// then runs without one.
	_ = w.rc.SetWriteDeadline(time.Now().Add(streamIdleTimeout))
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (w idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// clientIP returns the address to attribute a request to. A forwarded address
// is believed only when the direct peer is a trusted proxy. Then the rightmost
// X-Forwarded-For entry that is not itself trusted is used; a malformed entry
// stops the search and the proxy's address stands.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// logAuthFailure writes the structured failure event that Fail2ban reads (see
// deploy/fail2ban). It holds the client address, the reason and the path, but
// never the username, the password or the query string.
func logAuthFailure(ip, reason, path string) {
	logging.Warnf("auth failed ip=%s reason=%s path=%s", ip, reason, path)
}

// logCredentialFailure logs a refused API key or session in the same form as a
// failed password, so the Fail2ban filter matches it. A revoked key is a
// warning: someone is using a key that was taken out of service. The others
// are routine (an old phone, an expired key) and log at info.
func logCredentialFailure(ip, reason, path, extra string) {
	level := slog.LevelInfo
	if reason == "revoked-key" {
		level = slog.LevelWarn
	}
	logging.Logf(level, "auth failed ip=%s reason=%s path=%s%s", ip, reason, path, extra)
}
