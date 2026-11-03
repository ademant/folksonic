package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Only librarians may start a library scan. Other users get Subsonic error 50,
// and unauthenticated calls get error 40.
func TestStartScanRestrictedToLibrarians(t *testing.T) {
	s := newTestState(t)
	s.Progress = &scanner.Progress{}
	s.MusicDirs = []string{t.TempDir()}
	s.CoversDir = t.TempDir()
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create(ctx, s.Pool, "lib", "pw", users.RoleLibrarian, users.StatusActive); err != nil {
		t.Fatal(err)
	}

	call := func(query string) string {
		rec := httptest.NewRecorder()
		s.StartScan(rec, httptest.NewRequest(http.MethodGet, "/rest/startScan?"+query, nil))
		return rec.Body.String()
	}
	if body := call("u=bob&p=pw"); !strings.Contains(body, `"code":50`) {
		t.Fatalf("plain user could start a scan: %s", body)
	}
	if body := call("u=nobody&p=x"); !strings.Contains(body, `"code":40`) {
		t.Fatalf("unauthenticated scan not refused: %s", body)
	}
	if body := call("u=lib&p=pw"); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("librarian refused: %s", body)
	}
}
