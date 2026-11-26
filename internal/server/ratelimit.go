package server

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
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
//
// The same type limits requests to public begin endpoints (beginLimit,
// pollLimit), with its own burst and refill; there every request spends.
type authLimiter struct {
	mu      sync.Mutex
	clock   func() time.Time // nil means time.Now; tests set it
	entries map[string]*authEntry
	// burst and refill override authBurst and authRefill when set.
	burst, refill float64
}

func (l *authLimiter) rates() (burst, refill float64) {
	burst, refill = authBurst, authRefill
	if l.burst > 0 {
		burst = l.burst
	}
	if l.refill > 0 {
		refill = l.refill
	}
	return burst, refill
}

// authMaxEntries bounds memory, hard (#167). When the table is full, idle
// entries go first, then the entry that has spent least; a new address never
// grows the table past the bound.
const (
	authMaxEntries = 10000
	authIdle       = 10 * time.Minute
)

// limiterKey is the table key for a client address. An IPv6 client gets its
// /64, which one host or home network has in full, so cycling through its
// addresses does not give it fresh budgets or fill the table (#167).
func limiterKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Unmap().Is4() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

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
	burst, refill := l.rates()
	ip = limiterKey(ip)
	e, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= authMaxEntries {
			l.evict(now, burst, refill)
		}
		e = &authEntry{tokens: burst, last: now}
		l.entries[ip] = e
	}
	e.tokens = min(burst, e.tokens+now.Sub(e.last).Seconds()*refill)
	e.last = now
	return e
}

// evict makes room for one entry: every idle entry goes, and if none was
// idle, the one with the most budget left (refilled to now), which is the
// cheapest to forget (#167). Callers hold l.mu.
func (l *authLimiter) evict(now time.Time, burst, refill float64) {
	var best string
	bestTokens := -1.0
	removed := false
	for k, e := range l.entries {
		if now.Sub(e.last) > authIdle {
			delete(l.entries, k)
			removed = true
			continue
		}
		if t := min(burst, e.tokens+now.Sub(e.last).Seconds()*refill); t > bestTokens {
			best, bestTokens = k, t
		}
	}
	if !removed && best != "" {
		delete(l.entries, best)
	}
}

// allow reports whether ip may try a password now. It spends one token before
// the password is hashed, so parallel attempts cannot all pass the check
// before the first failure is counted (#109). A success gives the token back
// (succeed); a refused attempt keeps it spent, and fail counts the failure.
// When ip may not try, retryAfter says how long until the next attempt is allowed.
func (l *authLimiter) allow(ip string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip, l.now())
	if e.tokens >= 1 {
		e.tokens--
		return true, 0
	}
	_, refill := l.rates()
	wait := (1 - e.tokens) / refill
	return false, time.Duration(wait * float64(time.Second))
}

// fail records a failed attempt from ip and returns how long the caller should
// wait before answering. The token was spent by allow.
func (l *authLimiter) fail(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip, l.now())
	e.fails++
	return delayFor(e.fails)
}

// succeed clears the consecutive-failure count for ip and gives back the token
// that allow spent on this correct attempt.
func (l *authLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	burst, _ := l.rates()
	if e, ok := l.entries[limiterKey(ip)]; ok {
		e.fails = 0
		e.tokens = min(burst, e.tokens+1)
	}
}

// refund gives back the token spent by allow when no password was checked
// because the server was busy. The server's load is not the client's failure.
func (l *authLimiter) refund(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	burst, _ := l.rates()
	if e, ok := l.entries[limiterKey(ip)]; ok {
		e.tokens = min(burst, e.tokens+1)
	}
}

// Limits on the public endpoints that start a sign-in or a pairing (#168,
// #178): each request spends, successful or not. beginLimit allows 10 at
// once and one more every 5 seconds; pollLimit, for the device token poll,
// 30 at once and two a second, far above a client polling at the RFC 8628
// interval.
const (
	beginBurst  = 10
	beginRefill = 0.2
	pollBurst   = 30
	pollRefill  = 2
)

// throttled answers 429 with Retry-After when ip has spent its budget on l,
// and reports whether it did.
func (s *State) throttled(w http.ResponseWriter, r *http.Request, l *authLimiter) bool {
	ip := s.clientIP(r)
	ok, retry := l.allow(ip)
	if ok {
		return false
	}
	logAuthFailure(ip, "rate-limit", r.URL.Path)
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(retry)))
	writeAPIErrorCode(w, http.StatusTooManyRequests, codeRateLimited, "too many requests; wait before trying again")
	return true
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
	if reason == users.CodeKeyRevoked {
		level = slog.LevelWarn
	}
	logging.Logf(level, "auth failed ip=%s reason=%s path=%s%s", ip, reason, path, extra)
}
