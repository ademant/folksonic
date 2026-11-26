package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/users"
)

const importSample = `{"schema":1,"file":"t.flac","duration":200.5,"bpm":124.0,"bpm_confidence":2.66,` +
	`"beats":[0.0,0.5,1.0],"downbeatIndexes":[0]}`

// importFixture is a library of two real files with folksentia assigned to
// bpm and beats (offline only), the librarian "admin", the user "bob", and
// a key for each of them. The admin's key holds the analysis import grant.
func importFixture(t *testing.T) (s *State, adminKey, bobKey string) {
	t.Helper()
	s = newTestState(t)
	dir := t.TempDir()
	s.MusicDirs = []string{dir}
	s.uploadDir = t.TempDir()
	s.Analysis = analysis.NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia",
		Tools: map[string]config.AnalysisTool{"folksentia": {TimeoutDur: time.Minute}},
	}, "")
	if _, err := s.Pool.Exec(`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a');
		INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'Al', 'A', 'ar-1')`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		p := filepath.Join(dir, n+".flac")
		if err := os.WriteFile(p, []byte("audio "+n), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, duration_ms)
			VALUES (?, ?, ?, 'A', 'ar-1', 'Al', 'al-1', 'flac', 'audio/flac', 200000)`, "so-"+n, p, n); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	var err error
	if adminKey, err = users.CreateAPIKey(ctx, s.Pool, userIDOf(t, s, "admin"), "studio", 0); err != nil {
		t.Fatal(err)
	}
	if bobKey, err = users.CreateAPIKey(ctx, s.Pool, userIDOf(t, s, "bob"), "phone", 0); err != nil {
		t.Fatal(err)
	}
	keys, _ := users.ListAPIKeys(ctx, s.Pool, userIDOf(t, s, "admin"))
	if err := users.GrantAnalysisImport(ctx, s.Pool, userIDOf(t, s, "admin"), keys[0].ID); err != nil {
		t.Fatal(err)
	}
	return s, adminKey, bobKey
}

// importReq sends a request the way limitBody and the mux see it in the
// server. key goes into X-Api-Key; user, when set, signs in with a session.
func importReq(t *testing.T, s *State, method, path, key, user string, body []byte, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	if user != "" {
		tok, err := users.CreateSession(context.Background(), s.Pool, userIDOf(t, s, user), "")
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-ndjson")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	limitBody(s.routes()).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// fetchJobs returns the job list for all songs through the API.
func fetchJobs(t *testing.T, s *State, key string) analysis.JobsFile {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/analysis/jobs?all=1", nil)
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs: %d %s", rec.Code, rec.Body)
	}
	var f analysis.JobsFile
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func resultLines(t *testing.T, jobs []analysis.OfflineJob) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, j := range jobs {
		js, _ := json.Marshal(j)
		b.WriteString(`{"job":` + string(js) + `,"toolVersion":"folksentia 0.5.0","result":` + importSample + "}\n")
	}
	return b.Bytes()
}

func TestAnalysisJobsAndResultsRoundTrip(t *testing.T) {
	s, key, _ := importFixture(t)
	f := fetchJobs(t, s, key)
	if f.Format != analysis.JobsFormat || len(f.Jobs) != 2 {
		t.Fatalf("jobs: %+v", f)
	}
	body := resultLines(t, f.Jobs)
	body = append(body, []byte("not json\n")...)

	// A dry run reports and stores nothing.
	code, rep := importReq(t, s, "POST", "/api/analysis/results?dryRun=1", key, "", body, nil)
	if code != http.StatusOK || rep["applied"] != 2.0 || rep["changed"] != 2.0 || rep["rejected"] != 1.0 || rep["dryRun"] != true {
		t.Fatalf("dry run: %d %v", code, rep)
	}
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM songs WHERE bpm IS NOT NULL").Scan(&n); err != nil || n != 0 {
		t.Fatalf("dry run stored %d BPMs (%v)", n, err)
	}
	entries, _ := rep["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["reason"] != "not_json" || entries[0].(map[string]any)["status"] != "rejected" {
		t.Fatalf("entries: %v", rep["entries"])
	}

	// The real import, gzipped, stores both; a second one changes nothing.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(body)
	_ = zw.Close()
	code, rep = importReq(t, s, "POST", "/api/analysis/results", key, "", gz.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	if code != http.StatusOK || rep["applied"] != 2.0 || rep["changed"] != 2.0 {
		t.Fatalf("import: %d %v", code, rep)
	}
	if changed, _ := rep["changedSongs"].([]any); len(changed) != 2 {
		t.Fatalf("changedSongs: %v", rep["changedSongs"])
	}
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM songs WHERE bpm = 124 AND bpm_source = 'folksentia'").Scan(&n); err != nil || n != 2 {
		t.Fatalf("stored %d BPMs (%v)", n, err)
	}
	code, rep = importReq(t, s, "POST", "/api/analysis/results", key, "", body, nil)
	if code != http.StatusOK || rep["unchanged"] != 2.0 || rep["changed"] != 0.0 {
		t.Fatalf("repeat: %d %v", code, rep)
	}
	// The spool file is gone.
	if left, _ := os.ReadDir(s.uploadDir); len(left) != 0 {
		t.Fatalf("upload dir not cleaned: %v", left)
	}
}

func TestAnalysisImportStaleAndPending(t *testing.T) {
	s, key, _ := importFixture(t)
	f := fetchJobs(t, s, key)
	// The file changed on the server after the export.
	if err := os.WriteFile(filepath.Join(s.MusicDirs[0], "a.flac"), []byte("re-encoded audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, rep := importReq(t, s, "POST", "/api/analysis/results", key, "", resultLines(t, f.Jobs), nil)
	skipped, _ := rep["skipped"].(map[string]any)
	if code != http.StatusOK || rep["applied"] != 1.0 || skipped["stale"] != 1.0 {
		t.Fatalf("stale: %d %v", code, rep)
	}
	entries, _ := rep["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["songId"] != "so-a" || entries[0].(map[string]any)["reason"] != "stale" {
		t.Fatalf("entries: %v", rep["entries"])
	}
	// pending=1 now lists only the song still without results.
	req := httptest.NewRequest("GET", "/api/analysis/jobs?pending=1&features=bpm", nil)
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var pf analysis.JobsFile
	_ = json.Unmarshal(rec.Body.Bytes(), &pf)
	if rec.Code != http.StatusOK || len(pf.Jobs) != 1 || pf.Jobs[0].ID != "so-a" {
		t.Fatalf("pending: %d %s", rec.Code, rec.Body)
	}
}

func TestAnalysisImportAuth(t *testing.T) {
	s, key, bobKey := importFixture(t)
	ctx := context.Background()
	other, err := users.CreateAPIKey(ctx, s.Pool, userIDOf(t, s, "admin"), "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range [][2]string{{"GET", "/api/analysis/jobs?all=1"}, {"POST", "/api/analysis/results"}} {
		cases := []struct {
			name, key, user string
			want            int
			code            string
		}{
			{"anonymous", "", "", http.StatusUnauthorized, users.CodeCredentialsRequired},
			{"librarian key without grant", other, "", http.StatusForbidden, users.CodeAnalysisImportRequired},
			{"user key", bobKey, "", http.StatusForbidden, users.CodeAnalysisImportRequired},
			{"user session", "", "bob", http.StatusForbidden, ""},
			{"librarian session", "", "admin", http.StatusOK, ""},
			{"granted key", key, "", http.StatusOK, ""},
		}
		for _, c := range cases {
			code, body := importReq(t, s, ep[0], ep[1], c.key, c.user, nil, nil)
			if code != c.want || (c.code != "" && body["error"] != c.code) {
				t.Errorf("%s %s, %s: %d %v", ep[0], ep[1], c.name, code, body)
			}
		}
		// The granted key in the URL is refused: only the header counts.
		sep := "?"
		if strings.Contains(ep[1], "?") {
			sep = "&"
		}
		if code, body := importReq(t, s, ep[0], ep[1]+sep+"apiKey="+key, "", "", nil, nil); code != http.StatusForbidden ||
			body["error"] != users.CodeAnalysisImportRequired {
			t.Errorf("%s key in URL: %d %v", ep[1], code, body)
		}
	}
	// The grant does not open the library administration.
	if code, _ := importReq(t, s, "GET", "/api/library/stats", key, "", nil, nil); code != http.StatusForbidden {
		t.Errorf("granted key on /api/library/stats: %d", code)
	}
	// A librarian who becomes a user loses the endpoints with the same key.
	if _, err := s.Pool.Exec("UPDATE users SET role = ? WHERE username = 'admin'", users.RoleUser); err != nil {
		t.Fatal(err)
	}
	if code, _ := importReq(t, s, "GET", "/api/analysis/jobs?all=1", key, "", nil, nil); code != http.StatusForbidden {
		t.Errorf("demoted librarian's key: %d", code)
	}
}

func TestAnalysisResultsLimits(t *testing.T) {
	s, key, _ := importFixture(t)
	s.analysisUploadMaxMB = 1
	big := bytes.Repeat([]byte("x"), 1<<20+10)
	if code, body := importReq(t, s, "POST", "/api/analysis/results", key, "", big, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d %v", code, body)
	}
	// The limit applies after decompression too.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(big)
	_ = zw.Close()
	if code, body := importReq(t, s, "POST", "/api/analysis/results", key, "", gz.Bytes(), map[string]string{"Content-Encoding": "gzip"}); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large gzip body: %d %v", code, body)
	}
	if code, _ := importReq(t, s, "POST", "/api/analysis/results", key, "", []byte("{}"), map[string]string{"Content-Encoding": "br"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("brotli: %d", code)
	}
	if code, _ := importReq(t, s, "POST", "/api/analysis/results", key, "", []byte("a=b"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("form body: %d", code)
	}
	if code, _ := importReq(t, s, "POST", "/api/analysis/results", key, "", []byte("not gzip"), map[string]string{"Content-Encoding": "gzip"}); code != http.StatusBadRequest {
		t.Errorf("bad gzip: %d", code)
	}
	// Too many lines.
	many := bytes.Repeat([]byte("{}\n"), maxResultLines+1)
	if code, body := importReq(t, s, "POST", "/api/analysis/results", key, "", many, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("too many lines: %d %v", code, body)
	}
	// One import at a time.
	s.imports.mu.Lock()
	code, _ := importReq(t, s, "POST", "/api/analysis/results", key, "", []byte("{}\n"), nil)
	s.imports.mu.Unlock()
	if code != http.StatusConflict {
		t.Errorf("concurrent import: %d", code)
	}
	// The job list needs exactly one mode.
	for _, q := range []string{"", "?all=1&pending=1", "?all=1&features=key"} {
		if code, _ := importReq(t, s, "GET", "/api/analysis/jobs"+q, key, "", nil, nil); code != http.StatusBadRequest {
			t.Errorf("jobs%s: %d", q, code)
		}
	}
}

func TestAnalysisImportGrant(t *testing.T) {
	s, _, bobKey := importFixture(t)
	ctx := context.Background()
	admin := userIDOf(t, s, "admin")
	if _, err := users.CreateAPIKey(ctx, s.Pool, admin, "second", 0); err != nil {
		t.Fatal(err)
	}
	keys, _ := users.ListAPIKeys(ctx, s.Pool, admin)
	first, second := keys[0], keys[1]
	if !first.AnalysisImport || second.AnalysisImport {
		t.Fatalf("before: %+v", keys)
	}
	path := func(id int64) string { return "/api/keys/" + jsonNum(id) + "/analysis-import" }

	// Granting moves the grant to the second key.
	if code, body := apiCall(t, s, "admin", "POST", path(second.ID), ""); code != http.StatusOK || body["analysisImport"] != true {
		t.Fatalf("grant: %d %v", code, body)
	}
	_, list := apiCall(t, s, "admin", "GET", "/api/keys", "")
	if list["canGrantAnalysisImport"] != true {
		t.Fatalf("list: %v", list)
	}
	got := map[float64]bool{}
	for _, k := range list["keys"].([]any) {
		m := k.(map[string]any)
		got[m["id"].(float64)] = m["analysisImport"].(bool)
	}
	if got[float64(first.ID)] || !got[float64(second.ID)] {
		t.Fatalf("after grant: %v", got)
	}
	if code, _ := apiCall(t, s, "admin", "DELETE", path(second.ID), ""); code != http.StatusOK {
		t.Fatalf("remove: %d", code)
	}
	if keys, _ := users.ListAPIKeys(ctx, s.Pool, admin); keys[0].AnalysisImport || keys[1].AnalysisImport {
		t.Fatalf("after remove: %+v", keys)
	}

	// Only librarians, only with a session, only their own keys.
	bobKeys, _ := users.ListAPIKeys(ctx, s.Pool, userIDOf(t, s, "bob"))
	if code, _ := apiCall(t, s, "bob", "POST", path(bobKeys[0].ID), ""); code != http.StatusForbidden {
		t.Errorf("user grants: %d", code)
	}
	if code, _ := apiCall(t, s, "admin", "POST", path(bobKeys[0].ID), ""); code != http.StatusNotFound {
		t.Errorf("librarian grants another account's key: %d", code)
	}
	if code, body := keyCall(t, s, bobKey, true, "POST", path(bobKeys[0].ID), ""); code != http.StatusForbidden {
		t.Errorf("grant with a key: %d %v", code, body)
	}
	// A revoked key cannot get the grant.
	if err := users.RevokeAPIKey(ctx, s.Pool, admin, first.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCall(t, s, "admin", "POST", path(first.ID), ""); code != http.StatusNotFound {
		t.Errorf("grant to a revoked key: %d", code)
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }
