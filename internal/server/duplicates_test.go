package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// lcgFingerprint is a real Chromaprint encoding of 300 sub-fingerprints
// (see internal/analysis/fingerprint_test.go).
const lcgFingerprint = "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"

func TestAPIDuplicates(t *testing.T) {
	s := pagingFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	fp := func(value string) string {
		return `{"algorithm":"chromaprint","version":2,"durationSec":37.5,"value":"` + value + `"}`
	}
	for _, v := range [][2]string{
		{"so-1", fp(lcgFingerprint)}, {"so-2", fp(lcgFingerprint)}, {"so-3", fp(lcgFingerprint)},
		{"so-4", fp("AQAAAQE")}, // one sub-fingerprint: too short to compare
		{"so-5", `{"broken"`},   // unreadable values are skipped
	} {
		if _, err := s.Pool.Exec("INSERT INTO song_values (song_id, name, value, changed_ms) VALUES (?, 'fingerprint', ?, 1)", v[0], v[1]); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := apiCall(t, s, "", "GET", "/api/duplicates", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	// Nothing computed yet: 202, and the search starts in the background.
	code, body := apiCall(t, s, "bob", "GET", "/api/duplicates", "")
	if code != http.StatusAccepted {
		t.Fatalf("first request: %d %v", code, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for code != http.StatusOK && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		code, body = apiCall(t, s, "bob", "GET", "/api/duplicates", "")
	}
	groups, _ := body["groups"].([]any)
	if code != http.StatusOK || len(groups) != 1 || body["fingerprints"] != 4.0 {
		t.Fatalf("after the search: %d %v", code, body)
	}
	g := groups[0].(map[string]any)
	if ids := g["songIds"].([]any); len(ids) != 3 || ids[0] != "so-1" || ids[2] != "so-3" || g["similarity"] != 1.0 {
		t.Fatalf("group %v", g)
	}

	// Removed songs leave the served groups at once; a group of one goes.
	if _, err := s.Pool.Exec("DELETE FROM songs WHERE id IN ('so-2', 'so-3')"); err != nil {
		t.Fatal(err)
	}
	if _, body := apiCall(t, s, "bob", "GET", "/api/duplicates", ""); len(body["groups"].([]any)) != 0 {
		t.Fatalf("removed songs still grouped: %v", body)
	}

	// The fingerprint is served by /api/analysis as stored.
	if _, body := apiCall(t, s, "bob", "GET", "/api/analysis?id=so-1", ""); body["fingerprint"].(map[string]any)["value"] != lcgFingerprint {
		t.Fatalf("analysis fingerprint: %v", body["fingerprint"])
	}
	ext := call(t, s, "GET", "getOpenSubsonicExtensions", url.Values{}, nil).body
	if !strings.Contains(ext, `{"name":"tinysonicAnalysis","versions":[1,2]}`) {
		t.Fatalf("extension: %s", ext)
	}
}
