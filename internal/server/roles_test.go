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

// A role change made by the CLI applies to the next request without a restart.
func TestSetRoleTakesEffectImmediately(t *testing.T) {
	s := newTestState(t)
	s.Progress = &scanner.Progress{}
	s.MusicDirs = []string{t.TempDir()}
	s.CoversDir = t.TempDir()
	ctx := context.Background()
	bob, err := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	call := func() string {
		rec := httptest.NewRecorder()
		s.StartScan(rec, httptest.NewRequest(http.MethodGet, "/rest/startScan?u=bob&p=pw", nil))
		return rec.Body.String()
	}
	if body := call(); !strings.Contains(body, `"code":50`) {
		t.Fatalf("user could start a scan: %s", body)
	}
	if err := users.SetRole(ctx, s.Pool, bob.ID, users.RoleLibrarian); err != nil {
		t.Fatal(err)
	}
	if body := call(); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("new librarian refused: %s", body)
	}
}

// After a rename, Subsonic sign-in works with the new name only.
func TestRenameChangesSubsonicLogin(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	bob, err := users.Create(ctx, s.Pool, "bobb", "pw", users.RoleUser, users.StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.Rename(ctx, s.Pool, bob.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	ping := func(name string) string {
		rec := httptest.NewRecorder()
		s.Ping(rec, httptest.NewRequest(http.MethodGet, "/rest/ping?u="+name+"&p=pw", nil))
		return rec.Body.String()
	}
	if body := ping("bob"); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("new name refused: %s", body)
	}
	if body := ping("bobb"); !strings.Contains(body, `"code":40`) {
		t.Fatalf("old name still accepted: %s", body)
	}
}
