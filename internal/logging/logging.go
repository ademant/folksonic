// Package logging sets up the process-wide logger: a log/slog handler with a
// minimum level, which writes syslog priority prefixes instead of timestamps
// when stderr is connected to the systemd journal.
//
// Code logs through the printf-style helpers (Debugf, Infof, Warnf, Errorf),
// which format the message only when its level is enabled.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParseLevel parses a level name: debug, info, warn (or warning) or error,
// in any case. The empty string means info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
}

// Setup installs a handler writing to stderr at the given minimum level as
// the default slog logger. Output from the standard log package goes through
// it too, at info level.
func Setup(level slog.Level) {
	h := NewHandler(os.Stderr, level, underJournal(os.Stderr))
	slog.SetDefault(slog.New(h))
}

// NewHandler returns a handler that writes one line per record to w and
// drops records below level. With journal set, each line starts with the
// syslog priority (<7> debug … <3> error), which journald stores as the
// entry's priority, and has no timestamp, because journald records the time.
// Otherwise each line starts with the local time. The level name is written
// for every level except info.
func NewHandler(w io.Writer, level slog.Leveler, journal bool) slog.Handler {
	return &handler{out: &output{w: w}, level: level, journal: journal}
}

// Debugf logs at debug level.
func Debugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }

// Infof logs at info level.
func Infof(format string, args ...any) { logf(slog.LevelInfo, format, args...) }

// Warnf logs at warn level.
func Warnf(format string, args ...any) { logf(slog.LevelWarn, format, args...) }

// Errorf logs at error level.
func Errorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

// Logf logs at the given level.
func Logf(level slog.Level, format string, args ...any) { logf(level, format, args...) }

// Fatalf logs at error level and exits with status 1.
func Fatalf(format string, args ...any) {
	logf(slog.LevelError, format, args...)
	os.Exit(1)
}

// StdLogger returns a standard library logger that writes through the
// default slog logger at the given level, for APIs such as
// http.Server.ErrorLog that take a *log.Logger.
func StdLogger(level slog.Level) *log.Logger {
	return slog.NewLogLogger(slog.Default().Handler(), level)
}

func logf(level slog.Level, format string, args ...any) {
	l := slog.Default()
	ctx := context.Background()
	if !l.Enabled(ctx, level) {
		return
	}
	l.Log(ctx, level, fmt.Sprintf(format, args...))
}

// output serialises writes from handlers that share it.
type output struct {
	mu sync.Mutex
	w  io.Writer
}

type handler struct {
	out     *output
	level   slog.Leveler
	journal bool
	attrs   string // preformatted " key=value" pairs from WithAttrs
	group   string // key prefix from WithGroup, ending in "."
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if h.journal {
		b.WriteString(priority(r.Level))
	} else {
		t := r.Time
		if t.IsZero() {
			t = time.Now()
		}
		b.WriteString(t.Format("2006/01/02 15:04:05 "))
	}
	if name := levelName(r.Level); name != "" {
		b.WriteString(name)
		b.WriteByte(' ')
	}
	// A message spanning lines would become several journal entries, the
	// later ones without priority; keep every record on one line.
	b.WriteString(oneLine(r.Message))
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	b.WriteByte('\n')

	h.out.mu.Lock()
	defer h.out.mu.Unlock()
	_, err := io.WriteString(h.out.w, b.String())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	for _, a := range attrs {
		appendAttr(&b, h.group, a)
	}
	c := *h
	c.attrs += b.String()
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.group += name + "."
	return &c
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			appendAttr(b, p, g)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix + a.Key)
	b.WriteByte('=')
	v := oneLine(a.Value.String())
	if v == "" || strings.ContainsAny(v, " \"=") {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// priority returns the sd-daemon prefix for a level (see sd-daemon(3)).
func priority(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "<3>"
	case l >= slog.LevelWarn:
		return "<4>"
	case l >= slog.LevelInfo:
		return "<6>"
	default:
		return "<7>"
	}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return ""
	default:
		return "DEBUG"
	}
}
