package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// callForm runs handler h with a form-encoded POST body.
func callForm(t *testing.T, h http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rest/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Playlists belong to the account that created them. Another account cannot
// list, read, change or delete them.
func TestPlaylistsArePerUser(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	if _, err := users.Create(ctx, s.Pool, "alice", "pw-a", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	admin, _ := users.ByName(ctx, s.Pool, "admin")

	rec := callForm(t, s.CreatePlaylist, url.Values{"u": {"admin"}, "p": {"s3cret"}, "name": {"admin mix"}})
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("admin create playlist: %s", rec.Body.String())
	}
	mine, err := repoAllPlaylists(ctx, s.Pool, admin.ID)
	if err != nil || len(mine) != 1 {
		t.Fatalf("admin playlists: %v, %d", err, len(mine))
	}
	id := mine[0].ID

	rec = callForm(t, s.GetPlaylists, url.Values{"u": {"alice"}, "p": {"pw-a"}})
	if strings.Contains(rec.Body.String(), "admin mix") {
		t.Fatalf("alice lists admin's playlist: %s", rec.Body.String())
	}

	rec = callForm(t, s.GetPlaylist, url.Values{"u": {"alice"}, "p": {"pw-a"}, "id": {id}})
	if !strings.Contains(rec.Body.String(), "Playlist not found") {
		t.Fatalf("alice reads admin's playlist: %s", rec.Body.String())
	}

	rec = callForm(t, s.DeletePlaylist, url.Values{"u": {"alice"}, "p": {"pw-a"}, "id": {id}})
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("delete call failed unexpectedly: %s", rec.Body.String())
	}
	if still, _ := repoFindPlaylist(ctx, s.Pool, admin.ID, id); still == nil {
		t.Fatal("alice deleted admin's playlist")
	}
}

// Stars are kept per account: two accounts can star the same item, and
// unstarring removes only the caller's star.
func TestStarIsPerAccount(t *testing.T) {
	s := newTestState(t)
	ctx := context.Background()
	admin, _ := users.ByName(ctx, s.Pool, "admin")
	bob, _ := users.Create(ctx, s.Pool, "bob", "pw", users.RoleUser, users.StatusActive)
	if err := repoStar(ctx, s.Pool, admin.ID, "al-x", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := repoStar(ctx, s.Pool, bob.ID, "al-x", "t2"); err != nil {
		t.Fatalf("second account could not star the same item: %v", err)
	}
	if err := repoUnstar(ctx, s.Pool, bob.ID, "al-x"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM starred WHERE id = 'al-x' AND user_id = ?", admin.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unstar by bob removed admin's star (rows=%d)", n)
	}
}
