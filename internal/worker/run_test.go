package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFolksentia prints one folksentia output for any file.
func fakeFolksentia(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "folksentia")
	script := "#!/bin/sh\necho '{\"schema\":1,\"file\":\"x\",\"bpm\":120,\"bpm_confidence\":2}'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func validConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{
		Server: "https://cloud.example.org/folksonic", APIKey: "tsk_test", MusicRoot: t.TempDir(),
		Folksentia: Tool{Command: fakeFolksentia(t)},
	}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckRefusesUnsafeOrIncompleteSettings(t *testing.T) {
	bad := map[string]func(c *Config){
		"plain http to a remote host": func(c *Config) { c.Server = "http://cloud.example.org/folksonic" },
		"no scheme":                   func(c *Config) { c.Server = "cloud.example.org" },
		"no key":                      func(c *Config) { c.APIKey = "" },
		"relative music root":         func(c *Config) { c.MusicRoot = "music" },
		"batch too large":             func(c *Config) { c.Batch = 21 },
		"no folksentia command":       func(c *Config) { c.Folksentia.Command = "" },
		"bad memory limit":            func(c *Config) { c.Folksentia.MemoryLimit = "lots" },
	}
	for name, mutate := range bad {
		c := validConfig(t)
		mutate(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Plain http is fine for this machine's own server.
	c := validConfig(t)
	c.Server = "http://localhost:4534"
	if err := c.Check(); err != nil {
		t.Errorf("localhost over http: %v", err)
	}
}

func TestLocalPathStaysInTheMusicRoot(t *testing.T) {
	if got, err := localPath("/mnt/music", "Artist/Album/01.flac"); err != nil || got != "/mnt/music/Artist/Album/01.flac" {
		t.Errorf("normal path: %q %v", got, err)
	}
	for _, rel := range []string{"../etc/passwd", "/etc/passwd", "a/../../x"} {
		if _, err := localPath("/mnt/music", rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
}

// stubServer answers like the server's worker endpoints for one track.
type stubServer struct {
	mu        sync.Mutex
	jobsGiven int
	results   []map[string]any
	key       string
	job       map[string]any
	done      chan struct{}
}

func (st *stubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if r.Header.Get("X-Api-Key") != st.key {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/worker/jobs":
		if st.jobsGiven > 0 {
			_, _ = w.Write([]byte(`{"jobs":[],"leaseSeconds":1800}`))
			return
		}
		st.jobsGiven++
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{st.job}, "leaseSeconds": 1800})
	case "/api/worker/results":
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		st.results = append(st.results, m)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
		close(st.done)
	default:
		http.NotFound(w, r)
	}
}

func TestRunAnalysesAJobAndSendsTheResult(t *testing.T) {
	cfg := validConfig(t)
	// A track as the server describes it, with a file of the same size and mtime.
	rel := "Artist/01.flac"
	local := filepath.Join(cfg.MusicRoot, rel)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("audio data"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 123000000)
	if err := os.Chtimes(local, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	st := &stubServer{key: "tsk_test", done: make(chan struct{}), job: map[string]any{
		"id": "so-1", "path": rel, "size": int64(len("audio data")), "mtimeMs": mtime.UnixMilli(),
		"durationMs": 200000, "features": []string{"bpm"},
	}}
	srv := httptest.NewServer(http.StripPrefix("/folksonic", st))
	defer srv.Close()
	cfg.Server = srv.URL + "/folksonic"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		<-st.done
		cancel()
	}()
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.results) != 1 {
		t.Fatalf("results sent: %d", len(st.results))
	}
	res := st.results[0]
	if res["id"] != "so-1" || res["tool"] != "folksentia" || res["size"] != float64(len("audio data")) {
		t.Errorf("result: %v", res)
	}
	if out, _ := res["output"].(string); !strings.Contains(out, `"bpm":120`) {
		t.Errorf("output not sent: %v", res)
	}
	if _, ok := res["error"]; ok {
		t.Errorf("error set on a good run: %v", res["error"])
	}
}

func TestChangedFileIsReportedNotAnalysed(t *testing.T) {
	cfg := validConfig(t)
	local := filepath.Join(cfg.MusicRoot, "x.flac")
	if err := os.WriteFile(local, []byte("now longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &stubServer{key: "tsk_test", done: make(chan struct{}), job: map[string]any{
		"id": "so-2", "path": "x.flac", "size": int64(3), "mtimeMs": int64(1), "features": []string{"bpm"},
	}}
	srv := httptest.NewServer(http.StripPrefix("/folksonic", st))
	defer srv.Close()
	cfg.Server = srv.URL + "/folksonic"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { <-st.done; cancel() }()
	_ = Run(ctx, cfg)
	if len(st.results) != 1 || !strings.Contains(st.results[0]["error"].(string), "changed") {
		t.Fatalf("changed file: %v", st.results)
	}
	if _, ok := st.results[0]["output"]; ok {
		t.Error("a changed file was analysed")
	}
}

func TestRejectedKeyStopsTheWorker(t *testing.T) {
	cfg := validConfig(t)
	st := &stubServer{key: "tsk_other", done: make(chan struct{})}
	srv := httptest.NewServer(http.StripPrefix("/folksonic", st))
	defer srv.Close()
	cfg.Server = srv.URL + "/folksonic"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Run(ctx, cfg); err == nil || !strings.Contains(err.Error(), "rejected the worker key") {
		t.Fatalf("rejected key: %v", err)
	}
}

func TestRedeemExchangesThePairingToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pair/complete", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Token, Device string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Token != "tsp_good" || in.Device == "" {
			http.Error(w, `{"error":"invalid or expired pairing token"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiKey": "tsk_new", "username": "worker"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	key, err := Redeem(context.Background(), srv.URL, "tsp_good", "desk")
	if err != nil || key != "tsk_new" {
		t.Fatalf("redeem: %q %v", key, err)
	}
	if _, err := Redeem(context.Background(), srv.URL, "tsp_bad", "desk"); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Errorf("bad token: %v", err)
	}
}
