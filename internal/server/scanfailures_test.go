package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Librarians list the files the scanner could not read, with paths below
// the music folder hidden, and getScanStatus counts them (#219).
func TestAPILibraryScanFailed(t *testing.T) {
	s := pagingFixture(t)
	s.MusicDirs = []string{"/m"}
	s.Progress = &scanner.Progress{}
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "lib", "pw", users.RoleLibrarian, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	const path = "/api/library/scan/failed"
	if code, _ := apiCall(t, s, "", "GET", path, ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}
	if code, _ := apiCall(t, s, "bob", "GET", path, ""); code != http.StatusForbidden {
		t.Errorf("as user: %d", code)
	}
	if code, _ := apiCall(t, s, "lib", "GET", path+"?limit=1001", ""); code != http.StatusBadRequest {
		t.Errorf("limit 1001: %d", code)
	}
	if _, err := s.Pool.Exec(`INSERT INTO scan_failures (path, file_size, file_mtime_ms, reason, failed_at) VALUES
		('/m/A/notes.mp3', 14, 1, 'not an audio file', '2026-11-26T10:00:00Z'),
		('/m/B/bad.flac', 900, 2, 'parser failed: index out of range', '2026-11-26T11:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	code, body := apiCall(t, s, "lib", "GET", path+"?limit=1", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	files, _ := body["files"].([]any)
	if body["total"] != float64(2) || len(files) != 1 {
		t.Fatalf("total %v, files %v", body["total"], files)
	}
	f := files[0].(map[string]any)
	if f["path"] != "<music>/B/bad.flac" || f["reason"] != "parser failed: index out of range" || f["size"] != float64(900) {
		t.Errorf("newest entry: %v", f)
	}

	code, body = apiCall(t, s, "lib", "GET", "/rest/getScanStatus", "")
	if code != http.StatusOK {
		t.Fatalf("getScanStatus: %d", code)
	}
	st, _ := body["subsonic-response"].(map[string]any)["scanStatus"].(map[string]any)
	if st["scanFailures"] != float64(2) {
		t.Errorf("scanStatus: %v", st)
	}
}
