package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// workerState is the pagingFixture with bpm and beats on a folksentia worker,
// and song so-1 fingerprinted as a scan would have recorded it.
func workerState(t *testing.T) (*State, string, string) {
	t.Helper()
	s := pagingFixture(t)
	s.Analysis = analysis.NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {Worker: true, TimeoutDur: time.Minute}},
	}, "")
	ctx := context.Background()
	if _, err := s.Pool.Exec("UPDATE songs SET file_size = 1234, file_mtime_ms = 1700000000000 WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "worker", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	wk, err := users.CreateScopedAPIKey(ctx, s.Pool, workerUserID(t, s, "worker"), "worker: desk", users.ScopeWorker, 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := users.CreateAPIKey(ctx, s.Pool, workerUserID(t, s, "bob"), "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, wk, full
}

func workerUserID(t *testing.T, s *State, name string) int64 {
	t.Helper()
	u, err := users.ByName(context.Background(), s.Pool, name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// keyCall sends a request presenting key: in the X-Api-Key header, or in the
// apiKey query parameter when inHeader is false.
func keyCall(t *testing.T, s *State, key string, inHeader bool, method, path, body string) (int, map[string]any) {
	t.Helper()
	if key != "" && !inHeader {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "apiKey=" + key
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" && inHeader {
		req.Header.Set("X-Api-Key", key)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestWorkerKeyWorksOnlyForWorkerEndpoints(t *testing.T) {
	s, wk, full := workerState(t)

	// A worker key is no key anywhere else, in any form.
	if code, _ := keyCall(t, s, wk, true, "GET", "/api/library/stats", ""); code != http.StatusUnauthorized {
		t.Errorf("worker key on the library API: %d", code)
	}
	if code, _ := keyCall(t, s, wk, false, "GET", "/api/library/stats", ""); code != http.StatusUnauthorized {
		t.Errorf("worker key in the query string: %d", code)
	}
	code, body := keyCall(t, s, wk, true, "GET", "/rest/ping?f=json", "")
	if sr, _ := body["subsonic-response"].(map[string]any); code == http.StatusOK && sr["status"] == "ok" {
		t.Errorf("worker key opened a Subsonic endpoint: %v", body)
	}
	// Only the header counts for worker endpoints; the query string is refused.
	if code, _ := keyCall(t, s, wk, false, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`); code != http.StatusUnauthorized {
		t.Errorf("worker key in the query on /api/worker: %d", code)
	}
	// A full key cannot act as a worker.
	if code, _ := keyCall(t, s, full, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`); code != http.StatusForbidden {
		t.Errorf("full key on /api/worker: %d", code)
	}
	if code, _ := keyCall(t, s, "", true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`); code != http.StatusUnauthorized {
		t.Errorf("no key on /api/worker: %d", code)
	}
	// The worker key does work on its own endpoints.
	if code, _ := keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`); code != http.StatusOK {
		t.Errorf("worker key on /api/worker: %d", code)
	}
}

func TestWorkerJobsAreLeasedWithRelativePaths(t *testing.T) {
	s, wk, _ := workerState(t)

	if code, _ := keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"ffmpeg"}`); code != http.StatusBadRequest {
		t.Errorf("a tool the server runs itself: %d", code)
	}
	if code, _ := keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia","limit":99}`); code != http.StatusBadRequest {
		t.Errorf("limit 99: %d", code)
	}
	code, body := keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia","limit":5}`)
	if code != http.StatusOK {
		t.Fatalf("jobs: %d %v", code, body)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("jobs: %v", body)
	}
	job := jobs[0].(map[string]any)
	if job["id"] != "so-1" || job["path"] != "1.mp3" || job["size"] != float64(1234) || job["mtimeMs"] != float64(1700000000000) {
		t.Errorf("job: %v", job)
	}
	if strings.HasPrefix(job["path"].(string), "/") {
		t.Errorf("absolute path exposed: %v", job["path"])
	}
	if _, ok := body["leaseSeconds"]; !ok {
		t.Errorf("no lease length: %v", body)
	}
	// Leased: a second request does not get it.
	if _, body := keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`); len(body["jobs"].([]any)) != 0 {
		t.Errorf("leased track given out again: %v", body)
	}
}

func TestWorkerResultsNeedLeaseAndFingerprint(t *testing.T) {
	s, wk, _ := workerState(t)
	keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`)

	// The fingerprint from the job is required.
	stale := `{"id":"so-1","tool":"folksentia","size":1,"mtimeMs":1,"output":"{}"}`
	if code, _ := keyCall(t, s, wk, true, "POST", "/api/worker/results", stale); code != http.StatusConflict {
		t.Errorf("result for another file: %d", code)
	}
	// The lease was released with that answer, so the good one is refused too.
	good := `{"id":"so-1","tool":"folksentia","size":1234,"mtimeMs":1700000000000,"output":"{\"schema\":1,\"file\":\"x\",\"bpm\":124,\"bpm_confidence\":2.66}"}`
	if code, _ := keyCall(t, s, wk, true, "POST", "/api/worker/results", good); code != http.StatusConflict {
		t.Errorf("result after the lease was released: %d", code)
	}

	// Again, with a fresh lease, the answer is stored.
	s2, wk2, _ := workerState(t)
	keyCall(t, s2, wk2, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`)
	code, body := keyCall(t, s2, wk2, true, "POST", "/api/worker/results", good)
	if code != http.StatusOK || body["status"] != "stored" || body["changed"] != true {
		t.Fatalf("stored result: %d %v", code, body)
	}
	var bpm float64
	if err := s2.Pool.QueryRow("SELECT bpm FROM songs WHERE id = 'so-1'").Scan(&bpm); err != nil || bpm != 124 {
		t.Errorf("bpm %v %v", bpm, err)
	}
}

func TestWorkerResultBodyIsNotCutAtOneMegabyte(t *testing.T) {
	s, wk, _ := workerState(t)
	keyCall(t, s, wk, true, "POST", "/api/worker/jobs", `{"tool":"folksentia"}`)
	// A waveform and beat grid of a long track: more than the normal limit.
	big, _ := json.Marshal(map[string]any{
		"id": "so-1", "tool": "folksentia", "size": 1234, "mtimeMs": 1700000000000,
		"output": `{"schema":1,"file":"x","waveform":{"bins":2,"peaks":[` + strings.Repeat("1,", 700000) + `1]}}`,
	})
	if len(big) < 1<<20 {
		t.Fatal("test body is too small")
	}
	// The size limit is applied by limitBody around the whole server.
	req := httptest.NewRequest("POST", "/api/worker/results", strings.NewReader(string(big)))
	req.Header.Set("X-Api-Key", wk)
	rec := httptest.NewRecorder()
	limitBody(s.routes()).ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["status"] != "stored" {
		t.Errorf("large result: %d %v", rec.Code, body)
	}
}
