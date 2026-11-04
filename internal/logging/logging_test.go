package logging

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"Error":   slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("ParseLevel accepted an unknown level")
	}
}

func TestJournalFormat(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelDebug, true))
	l.Debug("scan: walking /m")
	l.Info("listening on :4533")
	l.Warn("passkeys disabled")
	l.Error("db: broken")
	want := "<7>DEBUG scan: walking /m\n" +
		"<6>listening on :4533\n" +
		"<4>WARN passkeys disabled\n" +
		"<3>ERROR db: broken\n"
	if got := buf.String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTerminalFormatHasTimestamp(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(&buf, slog.LevelInfo, false)).Warn("x")
	if !regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d WARN x\n$`).MatchString(buf.String()) {
		t.Errorf("unexpected line %q", buf.String())
	}
}

func TestMinimumLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelWarn, true))
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	if got := buf.String(); got != "<4>WARN w\n<3>ERROR e\n" {
		t.Errorf("got %q", got)
	}
}

func TestHelpersUseDefaultLogger(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(NewHandler(&buf, slog.LevelInfo, true)))

	Debugf("hidden %d", 1)
	Infof("scan complete: %d scanned", 5)
	Warnf("bpm %s: %v", "so-1", "no pulse")
	if got := buf.String(); got != "<6>scan complete: 5 scanned\n<4>WARN bpm so-1: no pulse\n" {
		t.Errorf("got %q", got)
	}
}

// A newline in a message (e.g. from an error text) must not start a second
// journal entry without priority.
func TestMessageStaysOnOneLine(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(&buf, slog.LevelInfo, true)).Error("ffmpeg: a\nb\r\nc")
	if got := buf.String(); got != "<3>ERROR ffmpeg: a b c\n" {
		t.Errorf("got %q", got)
	}
}

func TestAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewHandler(&buf, slog.LevelInfo, true)).With("client", "DanceSonic")
	l.WithGroup("req").Info("GET", "path", "/rest/ping", "note", "two words")
	got := strings.TrimSpace(buf.String())
	want := `<6>GET client=DanceSonic req.path=/rest/ping req.note="two words"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
