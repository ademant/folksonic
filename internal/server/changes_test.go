package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// feedState is newTestState with the settle margin off, so a write made just
// now is visible to a sync made just now.
func feedState(t *testing.T) *State {
	t.Helper()
	old := changeSettleMs
	changeSettleMs = 0
	t.Cleanup(func() { changeSettleMs = old })
	return newTestState(t)
}

// tickFeed makes the next write carry a later millisecond timestamp.
func tickFeed() { time.Sleep(3 * time.Millisecond) }

func seedFeed(t *testing.T, s *State, songs int) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		"INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')",
		"INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')",
	}
	for i := 1; i <= songs; i++ {
		stmts = append(stmts, fmt.Sprintf(
			"INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) "+
				"VALUES ('so-%d', '/m/%d.flac', 't%d', 'A', 'ar-1', 'B', 'al-1', 'flac', 'audio/flac', 1)", i, i, i))
	}
	for _, q := range stmts {
		if _, err := s.Pool.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// changesCall runs getLibraryChanges with form values. It returns the
// libraryChanges object (nil on an error) and the raw body.
func changesCall(t *testing.T, s *State, form url.Values) (map[string]any, string) {
	t.Helper()
	rec := callForm(t, s.GetLibraryChanges, form)
	var env map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad body %q: %v", rec.Body.String(), err)
	}
	lc, _ := env["subsonic-response"]["libraryChanges"].(map[string]any)
	return lc, rec.Body.String()
}

// listIDs returns the sorted ids in one list of the answer.
func listIDs(t *testing.T, lc map[string]any, key string) []string {
	t.Helper()
	arr, _ := lc[key].([]any)
	var out []string
	for _, x := range arr {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	sort.Strings(out)
	return out
}

func serverTimeOf(t *testing.T, lc map[string]any) string {
	t.Helper()
	v, ok := lc["serverTime"].(float64)
	if !ok {
		t.Fatalf("no serverTime in %v", lc)
	}
	return strconv.FormatInt(int64(v), 10)
}

func wantIDs(t *testing.T, lc map[string]any, key string, want ...string) {
	t.Helper()
	got := listIDs(t, lc, key)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}

func TestLibraryChangesFullThenIncremental(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, 2)
	auth := url.Values{"u": {"admin"}, "p": {"s3cret"}}

	lc, body := changesCall(t, s, auth)
	if lc == nil {
		t.Fatalf("no answer: %s", body)
	}
	if full, _ := lc["full"].(bool); !full {
		t.Fatal("request without since is not full")
	}
	wantIDs(t, lc, "artists", "ar-1")
	wantIDs(t, lc, "albums", "al-1")
	wantIDs(t, lc, "songs", "so-1", "so-2")
	st := serverTimeOf(t, lc)

	// Nothing changed: empty lists, a fresh serverTime, and not full.
	tickFeed()
	withSince := func(since string) url.Values {
		return url.Values{"u": {"admin"}, "p": {"s3cret"}, "since": {since}}
	}
	lc, _ = changesCall(t, s, withSince(st))
	if full, _ := lc["full"].(bool); full {
		t.Fatal("incremental answer reported full")
	}
	for _, key := range []string{"artists", "albums", "songs", "playlists", "starred", "deleted"} {
		wantIDs(t, lc, key)
	}
	st2 := serverTimeOf(t, lc)
	if st2 <= st {
		t.Fatalf("serverTime did not advance: %s then %s", st, st2)
	}

	// A tag change reaches the song, its album and its artist.
	tickFeed()
	if _, err := s.Pool.Exec("UPDATE songs SET title = 'changed' WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	lc, _ = changesCall(t, s, withSince(st2))
	wantIDs(t, lc, "songs", "so-1")
	wantIDs(t, lc, "albums", "al-1")
	wantIDs(t, lc, "artists", "ar-1")
	st3 := serverTimeOf(t, lc)

	// Removing a song is a tombstone; the album stays because it has a song left.
	tickFeed()
	if _, err := s.Pool.Exec("DELETE FROM songs WHERE id = 'so-2'"); err != nil {
		t.Fatal(err)
	}
	lc, _ = changesCall(t, s, withSince(st3))
	wantIDs(t, lc, "songs")
	wantIDs(t, lc, "albums", "al-1")
	deleted, _ := lc["deleted"].([]any)
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v, want one song", deleted)
	}
	if d := deleted[0].(map[string]any); d["type"] != "song" || d["id"] != "so-2" {
		t.Errorf("deleted entry = %v", d)
	}
}

func TestLibraryChangesPlaylistsAndStarsAreOwnerScoped(t *testing.T) {
	s := feedState(t)
	ctx := context.Background()
	seedFeed(t, s, 1)
	if _, err := users.Create(ctx, s.Pool, "alice", "pw-a", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	admin, err := users.ByName(ctx, s.Pool, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(
		"INSERT INTO playlists (id, name, created_at, updated_at, owner_id) VALUES ('pl-1', 'Mix', 't', 't', ?)", admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := repoStar(ctx, s.Pool, admin.ID, "so-1", "t"); err != nil {
		t.Fatal(err)
	}

	alice := url.Values{"u": {"alice"}, "p": {"pw-a"}}
	adminAuth := url.Values{"u": {"admin"}, "p": {"s3cret"}}

	lc, _ := changesCall(t, s, alice)
	wantIDs(t, lc, "playlists")
	wantIDs(t, lc, "starred")

	lc, _ = changesCall(t, s, adminAuth)
	wantIDs(t, lc, "playlists", "pl-1")
	starred, _ := lc["starred"].([]any)
	if len(starred) != 1 || starred[0].(map[string]any)["type"] != "song" {
		t.Fatalf("admin starred = %v, want one song", starred)
	}
	st := serverTimeOf(t, lc)

	// Unstarring is a tombstone for the same account only.
	tickFeed()
	if err := repoUnstar(ctx, s.Pool, admin.ID, "so-1"); err != nil {
		t.Fatal(err)
	}
	lc, _ = changesCall(t, s, url.Values{"u": {"alice"}, "p": {"pw-a"}, "since": {st}})
	if d, _ := lc["deleted"].([]any); len(d) != 0 {
		t.Fatalf("alice sees admin's unstar: %v", d)
	}
	lc, _ = changesCall(t, s, url.Values{"u": {"admin"}, "p": {"s3cret"}, "since": {st}})
	deleted, _ := lc["deleted"].([]any)
	if len(deleted) != 1 {
		t.Fatalf("admin deleted = %v, want the unstar", deleted)
	}
	if d := deleted[0].(map[string]any); d["type"] != "starred" || d["id"] != "so-1" {
		t.Errorf("unstar entry = %v", d)
	}
}

// A since older than the floor cannot be answered incrementally, because the
// tombstones it needs are gone. The answer is the full library instead.
func TestLibraryChangesStaleSinceIsFull(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, 1)
	auth := url.Values{"u": {"admin"}, "p": {"s3cret"}}
	lc, _ := changesCall(t, s, auth)
	st := serverTimeOf(t, lc)

	floor := time.Now().UnixMilli() + 1000
	if _, err := s.Pool.Exec(
		"INSERT INTO settings (key, value) VALUES ('change_floor_ms', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		strconv.FormatInt(floor, 10)); err != nil {
		t.Fatal(err)
	}
	lc, _ = changesCall(t, s, url.Values{"u": {"admin"}, "p": {"s3cret"}, "since": {st}})
	if full, _ := lc["full"].(bool); !full {
		t.Fatal("since before the floor was not answered full")
	}
	wantIDs(t, lc, "songs", "so-1")
	if d, _ := lc["deleted"].([]any); len(d) != 0 {
		t.Fatalf("full answer lists deletions: %v", d)
	}
}

// Pages of one answer share a snapshot and together hold every item once.
func TestLibraryChangesPagesWithCursor(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, 5)
	form := url.Values{"u": {"admin"}, "p": {"s3cret"}, "size": {"2"}}

	seen := map[string]int{}
	var snapshot string
	pages := 0
	for {
		lc, body := changesCall(t, s, form)
		if lc == nil {
			t.Fatalf("page %d failed: %s", pages, body)
		}
		pages++
		if got := serverTimeOf(t, lc); snapshot == "" {
			snapshot = got
		} else if got != snapshot {
			t.Fatalf("page %d serverTime %s, want the snapshot %s", pages, got, snapshot)
		}
		for _, key := range []string{"artists", "albums", "songs"} {
			for _, id := range listIDs(t, lc, key) {
				seen[id]++
			}
		}
		next, ok := lc["nextCursor"].(string)
		if !ok {
			break
		}
		form.Set("cursor", next)
		if pages > 10 {
			t.Fatal("cursor never ended")
		}
	}
	if pages < 4 {
		t.Errorf("%d pages for 7 items at size 2", pages)
	}
	if len(seen) != 7 {
		t.Errorf("saw %d distinct items, want 7: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times", id, n)
		}
	}
}

func TestLibraryChangesAuth(t *testing.T) {
	s := feedState(t)
	ctx := context.Background()
	seedFeed(t, s, 1)

	// Token auth is not supported: error 42, not a password failure.
	_, body := changesCall(t, s, url.Values{"u": {"admin"}, "t": {"abc"}, "s": {"xyz"}})
	if !strings.Contains(body, `"code":42`) {
		t.Fatalf("token auth: want error 42, got %s", body)
	}

	// A wrong password is error 40.
	_, body = changesCall(t, s, url.Values{"u": {"admin"}, "p": {"nope"}})
	if !strings.Contains(body, `"code":40`) {
		t.Fatalf("wrong password: want error 40, got %s", body)
	}

	// An API key from device pairing works.
	admin, err := users.ByName(ctx, s.Pool, "admin")
	if err != nil {
		t.Fatal(err)
	}
	token, err := users.CreatePairingToken(ctx, s.Pool, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := users.RedeemPairingToken(ctx, s.Pool, token, "phone")
	if err != nil {
		t.Fatal(err)
	}
	lc, body := changesCall(t, s, url.Values{"apiKey": {key}})
	if lc == nil {
		t.Fatalf("api key refused: %s", body)
	}
	wantIDs(t, lc, "songs", "so-1")
}

func TestLibraryChangesRejectsBadCursorAndSince(t *testing.T) {
	s := feedState(t)
	auth := url.Values{"u": {"admin"}, "p": {"s3cret"}}
	for _, extra := range []url.Values{
		{"cursor": {"not-a-cursor!"}},
		{"since": {"yesterday"}},
		{"since": {"-5"}},
	} {
		form := url.Values{}
		for k, v := range auth {
			form[k] = v
		}
		for k, v := range extra {
			form[k] = v
		}
		if lc, body := changesCall(t, s, form); lc != nil || !strings.Contains(body, `"status":"failed"`) {
			t.Errorf("%v: want a failed status, got %s", extra, body)
		}
	}
}

// itemOf returns the item of the entry with the given id in one list.
func itemOf(t *testing.T, lc map[string]any, key, id string) map[string]any {
	t.Helper()
	arr, _ := lc[key].([]any)
	for _, x := range arr {
		e := x.(map[string]any)
		if e["id"] == id {
			item, _ := e["item"].(map[string]any)
			return item
		}
	}
	t.Fatalf("no %s entry %s in %v", key, id, lc[key])
	return nil
}

func TestLibraryChangesIncludeItems(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, 2)
	auth := url.Values{"u": {"admin"}, "p": {"s3cret"}}

	// Without include, entries stay {id, updated}.
	lc, _ := changesCall(t, s, auth)
	if item := itemOf(t, lc, "songs", "so-1"); item != nil {
		t.Fatalf("item without include: %v", item)
	}

	form := url.Values{"u": {"admin"}, "p": {"s3cret"}, "include": {"items"}}
	lc, body := changesCall(t, s, form)
	if lc == nil {
		t.Fatalf("include=items failed: %s", body)
	}
	song := itemOf(t, lc, "songs", "so-1")
	if song["title"] != "t1" || song["mediaType"] != "song" || song["albumId"] != "al-1" {
		t.Errorf("song item: %v", song)
	}
	album := itemOf(t, lc, "albums", "al-1")
	if album["name"] != "B" || album["songCount"] != float64(2) {
		t.Errorf("album item: %v", album)
	}
	if _, ok := album["song"]; ok {
		t.Errorf("album item carries its songs: %v", album)
	}
	artist := itemOf(t, lc, "artists", "ar-1")
	if artist["name"] != "A" || artist["albumCount"] != float64(1) {
		t.Errorf("artist item: %v", artist)
	}

	// An incremental answer carries the changed song's new state.
	since := serverTimeOf(t, lc)
	tickFeed()
	if _, err := s.Pool.Exec("UPDATE songs SET title = 'renamed' WHERE id = 'so-2'"); err != nil {
		t.Fatal(err)
	}
	form.Set("since", since)
	lc, _ = changesCall(t, s, form)
	if got := itemOf(t, lc, "songs", "so-2")["title"]; got != "renamed" {
		t.Errorf("incremental song item title %v", got)
	}
}

// With include=items the answer is paged even without size, and every page
// read through the cursor carries items.
func TestLibraryChangesIncludeItemsPagesByDefault(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, maxPageSize+5)
	form := url.Values{"u": {"admin"}, "p": {"s3cret"}, "include": {"items"}}

	lc, body := changesCall(t, s, form)
	next, _ := lc["nextCursor"].(string)
	if next == "" {
		t.Fatalf("no nextCursor on a %d-row answer: %.200s", maxPageSize+7, body)
	}
	n := 0
	for _, k := range []string{"artists", "albums", "songs"} {
		arr, _ := lc[k].([]any)
		n += len(arr)
	}
	if n != maxPageSize {
		t.Fatalf("first page has %d entries, want %d", n, maxPageSize)
	}

	form.Set("cursor", next)
	lc, _ = changesCall(t, s, form)
	arr, _ := lc["songs"].([]any)
	if len(arr) == 0 {
		t.Fatal("second page is empty")
	}
	for _, x := range arr {
		if _, ok := x.(map[string]any)["item"]; !ok {
			t.Fatalf("second page entry without item: %v", x)
		}
	}
}

// Items follow tinysonicMinimal like every other song object.
func TestLibraryChangesIncludeItemsMinimal(t *testing.T) {
	s := feedState(t)
	seedFeed(t, s, 1)
	form := url.Values{"u": {"admin"}, "p": {"s3cret"}, "include": {"items"}}
	req := httptest.NewRequest(http.MethodPost, "/rest/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Prefer", preferMinimal)
	rec := httptest.NewRecorder()
	subsonicJSON(s.GetLibraryChanges)(rec, req)
	var env map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	lc := env["subsonic-response"]["libraryChanges"].(map[string]any)
	song := itemOf(t, lc, "songs", "so-1")
	if _, ok := song["path"]; ok {
		t.Errorf("minimal song item still has path: %v", song)
	}
	if song["title"] != "t1" {
		t.Errorf("minimal song item lost its title: %v", song)
	}
}

func TestLibraryChangesRejectsUnknownInclude(t *testing.T) {
	s := feedState(t)
	_, body := changesCall(t, s, url.Values{"u": {"admin"}, "p": {"s3cret"}, "include": {"everything"}})
	if !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("unknown include accepted: %s", body)
	}
}
