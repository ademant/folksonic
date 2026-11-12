package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// reqLog collects what the request log line reports beyond method, path,
// status and size: the client name, the authenticated user and the Subsonic
// error code of a failed envelope (which still answers HTTP 200).
// requestLogger creates one per request; authenticate and writeError fill it.
type reqLog struct {
	client string
	user   string
	errSet bool
	err    int
	// throttled is set when password attempts from the client are refused by
	// the failure limit; retryAfter is how long to wait.
	throttled  bool
	retryAfter time.Duration
	// refusal is the error code of a refused credential (see refusal.go); it
	// becomes the response's code when the request is answered.
	refusal string
}

type reqLogKey struct{}

func withReqLog(r *http.Request, l *reqLog) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), reqLogKey{}, l))
}

func reqLogFrom(ctx context.Context) *reqLog {
	l, _ := ctx.Value(reqLogKey{}).(*reqLog)
	return l
}

// noteCaller records the client name and the user that authenticate
// resolved (empty when it failed). The client comes from params, so form
// posts are covered too.
func noteCaller(r *http.Request, username string) {
	if l := reqLogFrom(r.Context()); l != nil {
		if username != "" {
			l.user = username
		}
		if c := params(r).Get("c"); c != "" {
			l.client = c
		}
	}
}

// noteError records a Subsonic error code on the request log of the response
// w belongs to. It follows wrapping writers (minimalWriter) down to the
// recordedResponse that requestLogger installed; without one (tests calling a
// handler directly) it does nothing.
func noteError(w http.ResponseWriter, code int) {
	for w != nil {
		switch t := w.(type) {
		case *recordedResponse:
			if t.log != nil {
				t.log.errSet, t.log.err = true, code
			}
			return
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
		default:
			return
		}
	}
}

// fields returns the optional " client=… user=… err=…" suffix of a log line.
func (l *reqLog) fields() string {
	var b strings.Builder
	if l.client != "" {
		b.WriteString(" client=" + logToken(l.client))
	}
	if l.user != "" {
		b.WriteString(" user=" + logToken(l.user))
	}
	if l.errSet {
		b.WriteString(" err=" + strconv.Itoa(l.err))
	}
	return b.String()
}

// maxLogToken caps client-supplied values in the log.
const maxLogToken = 64

// logToken makes a client-supplied value safe for one log field: letters,
// digits and ._-+/:@ are kept, anything else (spaces, control characters,
// quotes, =) becomes _, so a client cannot forge fields or lines. It is cut
// to maxLogToken characters.
func logToken(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxLogToken {
			break
		}
		if r == utf8.RuneError || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-+/:@", r)) {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
