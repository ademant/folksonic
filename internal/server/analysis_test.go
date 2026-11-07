package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// analysisGet calls GET /api/analysis as user.
func analysisGet(t *testing.T, s *State, user, id string) (int, map[string]any) {
	t.Helper()
	return apiCall(t, s, user, "GET", "/api/analysis?id="+url.QueryEscape(id), "")
}

func TestAPIAnalysis(t *testing.T) {
	s := pagingFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if code, _ := analysisGet(t, s, "", "so-1"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := apiCall(t, s, "bob", "GET", "/api/analysis", ""); code != http.StatusBadRequest {
		t.Fatalf("without id: %d", code)
	}
	if code, _ := analysisGet(t, s, "bob", "so-nope"); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}

	// Nothing known: every feature is null, the file has a version.
	code, body := analysisGet(t, s, "bob", "so-1")
	if code != http.StatusOK {
		t.Fatalf("plain song: %d %v", code, body)
	}
	for _, f := range []string{"waveform", "beats", "bpm", "key", "loudness", "silence", "fingerprint"} {
		if v, ok := body[f]; !ok || v != nil {
			t.Errorf("%s: %v (present %v)", f, v, ok)
		}
	}
	if v, _ := body["fileVersion"].(string); !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(v) {
		t.Fatalf("fileVersion %q", v)
	}
	before := body["fileVersion"]

	if _, err := s.Pool.Exec(`UPDATE songs SET bpm = 124, bpm_source = 'folksentia', bpm_confidence = 0.5,
			file_size = 9, file_mtime_ms = 1000 WHERE id = 'so-1';
		INSERT INTO song_values (song_id, name, value, changed_ms) VALUES
			('so-1', 'key', '{"tonic":"A","mode":"minor","confidence":null}', 1),
			('so-1', 'waveform', '{"bins":2,"peaks":[0,255]}', 1)`); err != nil {
		t.Fatal(err)
	}
	_, body = analysisGet(t, s, "bob", "so-1")
	if b := body["bpm"].(map[string]any); b["value"] != 124.0 || b["confidence"] != 0.5 {
		t.Fatalf("bpm %v", body["bpm"])
	}
	if k := body["key"].(map[string]any); k["tonic"] != "A" || k["mode"] != "minor" {
		t.Fatalf("key %v", body["key"])
	}
	if w := body["waveform"].(map[string]any); w["bins"] != 2.0 {
		t.Fatalf("waveform %v", body["waveform"])
	}
	if body["fileVersion"] == before {
		t.Fatal("fileVersion did not follow the file")
	}
	if _, ok := body["pending"]; ok {
		t.Fatal("pending without automatic analysis")
	}

	// Automatic analysis on, key assigned: a song without a result is 202.
	s.Analysis = analysis.NewPlan(config.Analysis{Key: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {Command: "/x/folksentia", TimeoutDur: time.Second}}}, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/analysis?id=so-2", nil)
	u, _ := users.ByName(context.Background(), s.Pool, "bob")
	tok, _ := users.CreateSession(context.Background(), s.Pool, u.ID)
	req.Header.Set("Authorization", "Bearer "+tok)
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Retry-After") != "30" || !strings.Contains(rec.Body.String(), `"pending":["key"]`) {
		t.Fatalf("pending song: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if _, err := s.Pool.Exec(`INSERT INTO analysis (song_id, feature, tool, analysed_ms, error) VALUES ('so-2', 'key', 'folksentia', 1, 'no audio')`); err != nil {
		t.Fatal(err)
	}
	if code, body := analysisGet(t, s, "bob", "so-2"); code != http.StatusOK || body["key"] != nil {
		t.Fatalf("failed analysis must not stay pending: %d %v", code, body)
	}

	ext := call(t, s, "GET", "getOpenSubsonicExtensions", url.Values{}, nil).body
	if !strings.Contains(ext, `"tinysonicAnalysis"`) {
		t.Fatalf("tinysonicAnalysis not advertised: %s", ext)
	}
}

func TestAPILibraryAnalysis(t *testing.T) {
	s := pagingFixture(t)
	s.Progress = &scanner.Progress{}
	if _, err := users.Create(context.Background(), s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCall(t, s, "bob", "POST", "/api/library/analysis", `{"all":true}`); code != http.StatusForbidden {
		t.Fatalf("as user: %d", code)
	}
	for _, body := range []string{`{}`, `{"all":true,"ids":["so-1"]}`, `{"all":true,"features":["key"]}`} {
		if code, _ := apiCall(t, s, "admin", "POST", "/api/library/analysis", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	s.FFmpegPath = "/nonexistent/ffmpeg"
	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/analysis", `{"ids":["so-1"]}`); code != http.StatusServiceUnavailable {
		t.Errorf("tool missing: %d", code)
	}
}

// With automatic analysis, a finished scan starts a pending pass, which runs
// the assigned tool and stores its values.
func TestAnalysisQueuedAfterScan(t *testing.T) {
	s := pagingFixture(t)
	s.Progress = &scanner.Progress{}
	dir := t.TempDir()
	tool := filepath.Join(dir, "folksentia")
	script := "#!/bin/sh\necho '{\"schema\":1,\"key\":\"D\",\"scale\":\"major\",\"key_strength\":0.7}'\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"so-1", "so-2", "so-3", "so-4", "so-5"} {
		p := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec("UPDATE songs SET path = ? WHERE id = ?", p, id); err != nil {
			t.Fatal(err)
		}
	}
	s.Analysis = analysis.NewPlan(config.Analysis{Key: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {Command: tool, TimeoutDur: 10 * time.Second}}}, "")

	// A scan in progress defers the pass to its end.
	if !s.Progress.Begin() {
		t.Fatal("could not claim the scan slot")
	}
	s.queueAnalysis()
	if s.jobs.running() {
		t.Fatal("analysis started during a scan")
	}
	s.Progress.SetAfterChange(s.queueAnalysis)
	if _, err := scanner.Scan(s.Pool, []string{t.TempDir()}, t.TempDir(), nil, s.Progress); err != nil {
		t.Fatal(err)
	}
	// The empty scan root prunes nothing outside it; wait for the job.
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := s.jobs.status()
		if st["kind"] == "analysis" && st["running"] == false {
			if st["mode"] != "pending" || st["total"] != 5 || st["updated"] != 5 || st["failed"] != 0 {
				t.Fatalf("job %v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no analysis after the scan: %v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var n int
	if err := s.Pool.QueryRow(`SELECT COUNT(*) FROM song_values WHERE name = 'key' AND value = '{"tonic":"D","mode":"major","confidence":0.7}'`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("stored keys: %d %v", n, err)
	}
	code, st := apiCall(t, s, "admin", "GET", "/api/library/stats", "")
	a, _ := st["analysis"].(map[string]any)
	if code != 200 || a["auto"] != true || a["features"].(map[string]any)["key"] != "folksentia" {
		t.Fatalf("stats analysis: %d %v", code, st["analysis"])
	}
}
