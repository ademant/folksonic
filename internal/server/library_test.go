package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/bpm"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

func TestSongLoudnessMeterBPMFields(t *testing.T) {
	s := pagingFixture(t)
	if _, err := s.Pool.Exec(`UPDATE songs SET rg_track_gain = 0.1015625, rg_album_gain = -3.37,
		rg_base_gain = 0, meter = '3/4', bpm = 164.6 WHERE id = 'so-4'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(`UPDATE songs SET rg_base_gain = 0 WHERE id = 'so-5'`); err != nil {
		t.Fatal(err)
	}
	for _, hdr := range []map[string]string{nil, minimalHdr} {
		songs := call(t, s, "GET", "getAlbum", url.Values{"id": {"al-3"}}, hdr).data["album"].(map[string]any)["song"].([]any)
		a, b := songs[0].(map[string]any), songs[1].(map[string]any)
		rg, ok := a["replayGain"].(map[string]any)
		if !ok || rg["trackGain"] != 0.101563 || rg["albumGain"] != -3.37 || rg["baseGain"] != 0.0 {
			t.Fatalf("replayGain: %v", a["replayGain"])
		}
		if _, ok := rg["trackPeak"]; ok {
			t.Fatal("unknown peak present")
		}
		if a["bpm"] != 165.0 || a["tinysonicMeter"] != "3/4" {
			t.Fatalf("bpm %v, meter %v", a["bpm"], a["tinysonicMeter"])
		}
		// Only a zero Opus output gain is known: no object, no bpm, no meter.
		for _, f := range []string{"replayGain", "bpm", "tinysonicMeter"} {
			if _, ok := b[f]; ok {
				t.Fatalf("song without values has %s: %v", f, b)
			}
		}
	}
	ext := call(t, s, "GET", "getOpenSubsonicExtensions", url.Values{}, nil).body
	if !strings.Contains(ext, `"tinysonicMeter"`) {
		t.Fatalf("tinysonicMeter not advertised: %s", ext)
	}
}

// apiCall sends an authenticated /api request with a session of the user.
func apiCall(t *testing.T, s *State, user, method, path, body string) (int, map[string]any) {
	t.Helper()
	var hdr string
	if user != "" {
		u, err := users.ByName(context.Background(), s.Pool, user)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := users.CreateSession(context.Background(), s.Pool, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		hdr = "Bearer " + tok
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if hdr != "" {
		req.Header.Set("Authorization", hdr)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestLibraryAPIAccess(t *testing.T) {
	s := pagingFixture(t)
	s.Progress = &scanner.Progress{}
	if _, err := users.Create(context.Background(), s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	for _, ep := range [][2]string{{"GET", "/api/library/stats"}, {"POST", "/api/library/scan"}, {"POST", "/api/library/bpm"}, {"POST", "/api/library/jobs/cancel"}} {
		if code, _ := apiCall(t, s, "", ep[0], ep[1], `{"mode":"missing"}`); code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d", ep[1], code)
		}
		if code, _ := apiCall(t, s, "bob", ep[0], ep[1], `{"mode":"missing"}`); code != http.StatusForbidden {
			t.Errorf("%s as user: %d", ep[1], code)
		}
	}

	if _, err := s.Pool.Exec(`UPDATE songs SET bpm = 120, bpm_source = 'tag' WHERE id = 'so-1';
		UPDATE songs SET bpm = 90, bpm_source = 'calc' WHERE id = 'so-2';
		INSERT INTO settings (key, value) VALUES ('last_scan', '2026-11-03T20:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	code, st := apiCall(t, s, "admin", "GET", "/api/library/stats", "")
	if code != 200 || st["songs"] != 5.0 || st["albums"] != 3.0 || st["artists"] != 3.0 || st["duration"] != 5.0 || st["lastScan"] != "2026-11-03T20:00:00Z" {
		t.Fatalf("stats %d: %v", code, st)
	}
	if b := st["bpm"].(map[string]any); b["tag"] != 1.0 || b["calc"] != 1.0 || b["missing"] != 3.0 {
		t.Fatalf("bpm coverage: %v", b)
	}

	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/bpm", `{"mode":"everything"}`); code != http.StatusBadRequest {
		t.Errorf("bad mode: %d", code)
	}
	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/jobs/cancel", ""); code != http.StatusConflict {
		t.Errorf("cancel without job: %d", code)
	}
	s.FFmpegPath = "/nonexistent/ffmpeg"
	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/bpm", `{"mode":"missing"}`); code != http.StatusServiceUnavailable {
		t.Errorf("without ffmpeg: %d", code)
	}
	code, st = apiCall(t, s, "admin", "GET", "/api/library/stats", "")
	if ff := st["ffmpeg"].(map[string]any); ff["available"] != false || ff["reason"] == nil {
		t.Errorf("ffmpeg status: %v", ff)
	}

	// While a scan runs, neither a second scan nor a BPM job may start.
	s.FFmpegPath = ""
	if !s.Progress.Begin() {
		t.Fatal("could not claim the scan slot")
	}
	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/scan", ""); code != http.StatusConflict {
		t.Errorf("second scan: %d", code)
	}
	if _, err := bpm.FindFFmpeg(""); err == nil {
		if code, _ := apiCall(t, s, "admin", "POST", "/api/library/bpm", `{"mode":"missing"}`); code != http.StatusConflict {
			t.Errorf("bpm during scan: %d", code)
		}
	}
}

// With a real ffmpeg: the job fills missing BPM from the audio, never touches
// tag values, records the change for tinysonicLibraryInfo, and blocks scans.
func TestBPMJobEndToEnd(t *testing.T) {
	ffmpeg, err := bpm.FindFFmpeg("")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	click := filepath.Join(dir, "click.flac")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i",
		"aevalsrc='if(lt(mod(t,0.5),0.03),sin(2*PI*1000*t),0)':s=22050:d=40", click)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate audio: %v %s", err, out)
	}
	s := pagingFixture(t)
	s.Progress = &scanner.Progress{}
	if _, err := s.Pool.Exec(`UPDATE songs SET duration_ms = 40000;
		UPDATE songs SET bpm = 77, bpm_source = 'tag' WHERE id = 'so-1'`); err != nil {
		t.Fatal(err)
	}
	// Paths are unique: each song gets its own copy of the file.
	for _, id := range []string{"so-1", "so-2", "so-3", "so-4", "so-5"} {
		p := filepath.Join(dir, id+".flac")
		if out, err := exec.Command("cp", click, p).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if _, err := s.Pool.Exec("UPDATE songs SET path = ? WHERE id = ?", p, id); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := apiCall(t, s, "admin", "POST", "/api/library/bpm", `{"mode":"missing"}`); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, _ := apiCall(t, s, "admin", "POST", "/api/library/scan", ""); code != http.StatusConflict {
		t.Errorf("scan during bpm job: %d", code)
	}
	deadline := time.Now().Add(60 * time.Second)
	for s.jobs.running() {
		if time.Now().After(deadline) {
			t.Fatal("job did not finish")
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := s.jobs.status()
	if st["total"] != 4 || st["updated"] != 4 || st["failed"] != 0 {
		t.Fatalf("job: %v", st)
	}
	rows, err := s.Pool.Query("SELECT id, bpm, bpm_source, bpm_confidence FROM songs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, src string
		var b float64
		var conf sql.NullFloat64
		if err := rows.Scan(&id, &b, &src, &conf); err != nil {
			t.Fatal(err)
		}
		if id == "so-1" {
			if b != 77 || src != "tag" {
				t.Fatalf("tag value changed: %v %s", b, src)
			}
			continue
		}
		if src != "calc" || math.Abs(b-120) > 2 || !conf.Valid {
			t.Fatalf("%s: bpm %v (%s, conf %v), want ~120 calc", id, b, src, conf)
		}
	}
	var mod string
	if err := s.Pool.QueryRow("SELECT value FROM settings WHERE key = ?", scanner.LibraryModifiedKey).Scan(&mod); err != nil {
		t.Fatalf("library change not recorded: %v", err)
	}
}
