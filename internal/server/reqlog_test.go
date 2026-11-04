package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// captureLog sends the default logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(logging.NewHandler(&buf, slog.LevelDebug, true)))
	return &buf
}

// logLine runs one request through requestLogger and subsonicJSON, as the
// router does, and returns the log line.
func logLine(t *testing.T, s *State, method, target string, form url.Values, prefer bool) string {
	t.Helper()
	buf := captureLog(t)
	h := requestLogger(subsonicJSON(s.GetLibraryChanges))
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if prefer {
		req.Header.Set("Prefer", preferMinimal)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return strings.TrimSpace(buf.String())
}

func TestRequestLogNamesClientAndUser(t *testing.T) {
	s := feedState(t)
	line := logLine(t, s, http.MethodGet, "/rest/getLibraryChanges?u=admin&p=s3cret&c=DanceSonic&v=1.16.1&f=json", nil, false)
	if !strings.HasSuffix(line, " client=DanceSonic user=admin") {
		t.Fatalf("got %q", line)
	}
	if strings.Contains(line, "s3cret") || strings.Contains(line, "?") {
		t.Fatalf("query string reached the log: %q", line)
	}
}

// A form post carries c in the body, not the URL.
func TestRequestLogReadsClientFromFormPost(t *testing.T) {
	s := feedState(t)
	line := logLine(t, s, http.MethodPost, "/rest/getLibraryChanges",
		url.Values{"u": {"admin"}, "p": {"s3cret"}, "c": {"web"}}, false)
	if !strings.HasSuffix(line, " client=web user=admin") {
		t.Fatalf("got %q", line)
	}
}

// Subsonic failures answer HTTP 200; the log shows the error code, also
// through the minimal-response writer.
func TestRequestLogShowsSubsonicError(t *testing.T) {
	s := feedState(t)
	for _, prefer := range []bool{false, true} {
		line := logLine(t, s, http.MethodGet, "/rest/getLibraryChanges?u=admin&p=wrong&c=app", nil, prefer)
		if !strings.Contains(line, " 200 ") || !strings.HasSuffix(line, " client=app err=40") {
			t.Fatalf("prefer=%v: got %q", prefer, line)
		}
	}
	line := logLine(t, s, http.MethodGet, "/rest/getLibraryChanges?u=admin&t=x&s=y", nil, false)
	if !strings.HasSuffix(line, " err=42") || strings.Contains(line, "user=") {
		t.Fatalf("token auth: got %q", line)
	}
}

func TestRequestLogWithoutCallerHasNoFields(t *testing.T) {
	buf := captureLog(t)
	h := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "<6>GET /nope 404 0B ") || strings.Contains(line, "=") {
		t.Fatalf("got %q", line)
	}
}

func TestLogToken(t *testing.T) {
	for in, want := range map[string]string{
		"DanceSonic":             "DanceSonic",
		"DSub 5.4":               "DSub_5.4",
		"evil\n<3>ERROR forged":  "evil__3_ERROR_forged",
		`a"b=c`:                  "a_b_c",
		"Stöd/1.0+beta:x@y":      "Stöd/1.0+beta:x@y",
		strings.Repeat("x", 100): strings.Repeat("x", maxLogToken),
		"bad\xffutf8":            "bad_utf8",
	} {
		if got := logToken(in); got != want {
			t.Errorf("logToken(%q) = %q, want %q", in, got, want)
		}
	}
}
