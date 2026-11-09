package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

func TestProgressAndSummaryLines(t *testing.T) {
	if got := progressLine("analysis", 1200, 3, 4138, 4.1, "so-9"); got !=
		"analysis: 1200 done, 3 failed, 4138 pending, 4.1 songs/min, current=so-9" {
		t.Errorf("progress: %q", got)
	}
	if got := summaryLine("analysis", 10, 1, 8, 90*time.Second, ""); got !=
		"analysis: finished, 10 done, 1 failed, 8 updated in 1m30s" {
		t.Errorf("summary: %q", got)
	}
	if got := summaryLine("bpm", 2, 0, 2, 3*time.Second, "stopped"); got !=
		"bpm: finished, 2 done, 0 failed, 2 updated in 3s (stopped)" {
		t.Errorf("summary with reason: %q", got)
	}
}

func TestSongsPerMinute(t *testing.T) {
	if got := songsPerMinute(0, time.Minute); got != 0 {
		t.Errorf("no songs: %v", got)
	}
	if got := songsPerMinute(5, 500*time.Millisecond); got != 0 {
		t.Errorf("under a second: %v", got)
	}
	if got := songsPerMinute(41, 10*time.Minute); got != 4.1 {
		t.Errorf("41 in 10 min: %v", got)
	}
}

func TestJobStatusWhileRunning(t *testing.T) {
	var j jobRunner
	j.kind, j.mode, j.active = "analysis", "pending", true
	j.total, j.done, j.failed = 50, 10, 1
	j.startedAt = time.Now().Add(-2 * time.Minute)
	j.current, j.lastErr = "so-7", "so-3 folksentia: no audio decoded"

	st := j.status()
	if st["pending"] != 40 || st["current"] != "so-7" || st["lastError"] != "so-3 folksentia: no audio decoded" {
		t.Errorf("running status: %v", st)
	}
	if rate, _ := st["songsPerMinute"].(float64); rate < 4.9 || rate > 5.1 {
		t.Errorf("rate: %v", st["songsPerMinute"])
	}

	j.active, j.finished = false, "2026-11-09T10:00:00Z"
	st = j.status()
	if _, ok := st["pending"]; ok {
		t.Errorf("pending set after the job ended: %v", st)
	}
	if st["lastError"] != "so-3 folksentia: no audio decoded" {
		t.Errorf("last error lost after the job ended: %v", st["lastError"])
	}
}

func TestAPILibraryFailed(t *testing.T) {
	s := pagingFixture(t)
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "lib", "pw", users.RoleLibrarian, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCall(t, s, "", "GET", "/api/library/analysis/failed", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}
	if code, _ := apiCall(t, s, "bob", "GET", "/api/library/analysis/failed", ""); code != http.StatusForbidden {
		t.Errorf("as user: %d", code)
	}
	if code, _ := apiCall(t, s, "lib", "GET", "/api/library/analysis/failed?limit=0", ""); code != http.StatusBadRequest {
		t.Errorf("limit 0: %d", code)
	}

	// Two tools failed on so-1, one on so-2, and so-3 has a result.
	if _, err := s.Pool.Exec(`INSERT INTO analysis (song_id, feature, tool, analysed_ms, error) VALUES
		('so-1', 'beats', 'folksentia', 2000, 'folksentia: no audio decoded'),
		('so-1', 'key', 'folksentia', 2000, 'folksentia: no audio decoded'),
		('so-1', 'loudness', 'ffmpeg', 2100, '/m/Alpha/One/01.flac: Invalid data'),
		('so-2', 'bpm', 'builtin', 1000, 'ffmpeg: timed out after 2m0s; /mnt/other: x'),
		('so-3', 'bpm', 'builtin', 3000, NULL)`); err != nil {
		t.Fatal(err)
	}
	code, body := apiCall(t, s, "lib", "GET", "/api/library/analysis/failed", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	if body["total"] != float64(2) {
		t.Errorf("total: %v", body["total"])
	}
	songs, _ := body["songs"].([]any)
	if len(songs) != 3 {
		t.Fatalf("entries: %d %v", len(songs), songs)
	}
	// Newest first; the ffmpeg error lost the server path.
	first := songs[0].(map[string]any)
	if first["id"] != "so-1" || first["tool"] != "ffmpeg" || first["error"] != "<music>/Alpha/One/01.flac: Invalid data" {
		t.Errorf("first entry: %v", first)
	}
	for _, e := range songs {
		if e.(map[string]any)["id"] == "so-2" && e.(map[string]any)["error"] != "ffmpeg: timed out after 2m0s; /mnt/other: x" {
			t.Errorf("a path outside the music folder changed: %v", e)
		}
	}
	if _, ok := first["analysedAt"].(string); !ok {
		t.Errorf("analysedAt: %v", first["analysedAt"])
	}

	code, body = apiCall(t, s, "lib", "GET", "/api/library/analysis/failed?limit=1", "")
	if songs, _ := body["songs"].([]any); code != http.StatusOK || len(songs) != 1 || body["total"] != float64(2) {
		t.Errorf("limit 1: %d %v", code, body)
	}
}

func TestStatsCountFailedSongs(t *testing.T) {
	s := pagingFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "lib", "pw", users.RoleLibrarian, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(`INSERT INTO analysis (song_id, feature, tool, analysed_ms, error) VALUES
		('so-1', 'beats', 'folksentia', 1, 'x'), ('so-1', 'key', 'folksentia', 1, 'x'), ('so-2', 'key', 'folksentia', 1, NULL)`); err != nil {
		t.Fatal(err)
	}
	code, body := apiCall(t, s, "lib", "GET", "/api/library/stats", "")
	if code != http.StatusOK || body["failedSongs"] != float64(1) {
		t.Errorf("failedSongs: %d %v", code, body["failedSongs"])
	}
}
