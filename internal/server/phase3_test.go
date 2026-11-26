package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// newKeyFor returns a new API key for the named user of the fixture.
func newKeyFor(t *testing.T, s *State, name string) string {
	t.Helper()
	key, err := users.CreateAPIKey(context.Background(), s.Pool, userIDOf(t, s, name), "desk", 0)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// subsonicBody decodes a Subsonic JSON answer and returns its response object.
func subsonicBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	sr, ok := top["subsonic-response"].(map[string]any)
	if !ok {
		t.Fatalf("no subsonic-response in %s", body)
	}
	return sr
}

// idsIn returns the "id" values of a list of Child objects.
func idsIn(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	var ids []string
	for _, item := range list {
		m, _ := item.(map[string]any)
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// assertIDs fails unless got equals want, in order.
func assertIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// errorCode returns the Subsonic error code of an answer, or -1 when ok.
func errorCode(t *testing.T, body string) int {
	t.Helper()
	sr := subsonicBody(t, body)
	if sr["status"] == "ok" {
		return -1
	}
	e, _ := sr["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return int(code)
}

// #137: frequent and recent use the caller's own play counts, and leave out
// albums without plays.
func TestAlbumListFrequentCountsPlays(t *testing.T) {
	s, _, key := keyFixture(t)
	// Distinct times: two plays of one song in the same millisecond are one
	// play (the dedupe key), which made this test flaky.
	scrobble(t, s, key, "id=so-4&time=1000") // al-3
	scrobble(t, s, key, "id=so-4&time=2000")
	scrobble(t, s, key, "id=so-1&time=3000") // al-1
	body := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=frequent", key, true).Body.String()
	albums := subsonicBody(t, body)["albumList2"].(map[string]any)["album"]
	assertIDs(t, "frequent", idsIn(t, albums), []string{"al-3", "al-1"})
}

func TestAlbumListRecentOrdersByLastPlay(t *testing.T) {
	s, _, key := keyFixture(t)
	scrobble(t, s, key, "id=so-4&time=1700000100000") // al-3, later
	scrobble(t, s, key, "id=so-1&time=1700000000000") // al-1, earlier
	body := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=recent", key, true).Body.String()
	albums := subsonicBody(t, body)["albumList2"].(map[string]any)["album"]
	assertIDs(t, "recent", idsIn(t, albums), []string{"al-3", "al-1"})
}

// The counts are per user: a user with no plays gets an empty frequent list.
func TestAlbumListFrequentIsPerUser(t *testing.T) {
	s, _, bobKey := keyFixture(t)
	scrobble(t, s, bobKey, "id=so-1")
	workerKey := newKeyFor(t, s, "worker")
	body := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=frequent", workerKey, true).Body.String()
	albums := subsonicBody(t, body)["albumList2"].(map[string]any)["album"]
	assertIDs(t, "worker's frequent", idsIn(t, albums), nil)
}

// #144: byYear is an ascending range, or descending when fromYear > toYear.
func TestAlbumListByYear(t *testing.T) {
	s, _, key := keyFixture(t)
	up := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=byYear&fromYear=2001&toYear=2002", key, true).Body.String()
	assertIDs(t, "byYear up", idsIn(t, subsonicBody(t, up)["albumList2"].(map[string]any)["album"]), []string{"al-1", "al-2"})
	down := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=byYear&fromYear=2002&toYear=2001", key, true).Body.String()
	assertIDs(t, "byYear down", idsIn(t, subsonicBody(t, down)["albumList2"].(map[string]any)["album"]), []string{"al-2", "al-1"})
}

// #144: byGenre matches the songs' genre without regard to case; an album
// is listed under every genre its songs carry.
func TestAlbumListByGenre(t *testing.T) {
	s, _, key := keyFixture(t)
	body := getWith(t, s.routes(), "/rest/getAlbumList2.view?type=byGenre&genre=folk", key, true).Body.String()
	assertIDs(t, "byGenre", idsIn(t, subsonicBody(t, body)["albumList2"].(map[string]any)["album"]), []string{"al-1", "al-3", "al-2"})
}

// #144: a list type that is not known, or a list without its parameters, is an
// error, not an alphabetical list.
func TestAlbumListRejectsBadParameters(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, path := range []string{
		"/rest/getAlbumList2.view?type=nonsense",
		"/rest/getAlbumList2.view?type=byYear",
		"/rest/getAlbumList2.view?type=byGenre",
	} {
		body := getWith(t, s.routes(), path, key, true).Body.String()
		if code := errorCode(t, body); code != 10 {
			t.Errorf("%s: code %d, want 10: %s", path, code, body)
		}
	}
}

// #146: getTopSongs orders the artist's songs by the caller's play count and
// leaves out songs never played.
func TestTopSongsByPlayCount(t *testing.T) {
	s, _, key := keyFixture(t)
	// Distinct times: plays of one song in the same millisecond are one play.
	for i, q := range []string{"id=so-2", "id=so-2", "id=so-2", "id=so-1"} {
		scrobble(t, s, key, q+"&time="+strconv.Itoa(1000*(i+1)))
	}
	body := getWith(t, s.routes(), "/rest/getTopSongs.view?artist=Alpha", key, true).Body.String()
	songs := subsonicBody(t, body)["topSongs"].(map[string]any)["song"]
	assertIDs(t, "topSongs", idsIn(t, songs), []string{"so-2", "so-1"})
}

func TestTopSongsNeedsAnArtist(t *testing.T) {
	s, _, key := keyFixture(t)
	body := getWith(t, s.routes(), "/rest/getTopSongs.view", key, true).Body.String()
	if code := errorCode(t, body); code != 10 {
		t.Fatalf("no artist: code %d, want 10: %s", code, body)
	}
}

// #145: similar songs are within 3 % of the seed's tempo, nearest first; the
// seed itself and songs without a BPM are left out.
func TestSimilarSongsByTempo(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, q := range []string{
		"UPDATE songs SET bpm = 120 WHERE id = 'so-1'",
		"UPDATE songs SET bpm = 121 WHERE id = 'so-2'", // within 3 %
		"UPDATE songs SET bpm = 130 WHERE id = 'so-3'", // outside
		"UPDATE songs SET bpm = 118 WHERE id = 'so-4'", // within 3 %, further
	} {
		if _, err := s.Pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	body := getWith(t, s.routes(), "/rest/getSimilarSongs2.view?id=so-1", key, true).Body.String()
	songs := subsonicBody(t, body)["similarSongs2"].(map[string]any)["song"]
	assertIDs(t, "similar to so-1", idsIn(t, songs), []string{"so-2", "so-4"})

	// getSimilarSongs answers under its own key.
	body = getWith(t, s.routes(), "/rest/getSimilarSongs.view?id=so-1", key, true).Body.String()
	if _, ok := subsonicBody(t, body)["similarSongs"]; !ok {
		t.Fatalf("getSimilarSongs has no similarSongs key: %s", body)
	}
}

// A seed without a BPM gives no songs, rather than a guess.
func TestSimilarSongsWithoutSeedTempoIsEmpty(t *testing.T) {
	s, _, key := keyFixture(t)
	body := getWith(t, s.routes(), "/rest/getSimilarSongs2.view?id=so-5", key, true).Body.String()
	songs := subsonicBody(t, body)["similarSongs2"].(map[string]any)["song"]
	assertIDs(t, "similar to a song without BPM", idsIn(t, songs), nil)
}

// #147: updateNowPlaying reports a player's song; getNowPlaying lists the
// caller's players only; a report expires after 30 minutes.
func TestNowPlayingPerPlayerAndExpiry(t *testing.T) {
	s, _, bobKey := keyFixture(t)
	bobID := userIDOf(t, s, "bob")
	getWith(t, s.routes(), "/rest/updateNowPlaying.view?id=so-1&playerId=phone", bobKey, true)
	getWith(t, s.routes(), "/rest/updateNowPlaying.view?id=so-2&playerId=desk", bobKey, true)
	// A later report from the same player replaces the earlier one.
	getWith(t, s.routes(), "/rest/updateNowPlaying.view?id=so-3&playerId=phone", bobKey, true)

	body := getWith(t, s.routes(), "/rest/getNowPlaying.view", bobKey, true).Body.String()
	entries := subsonicBody(t, body)["nowPlaying"].(map[string]any)["entry"]
	assertIDs(t, "bob's now playing", idsIn(t, entries), []string{"so-3", "so-2"})

	// Thirty minutes on, the reports have expired.
	if live := s.nowPlaying.list(bobID, time.Now().Add(nowPlayingTTL+time.Minute)); len(live) != 0 {
		t.Fatalf("reports after the TTL: %v", live)
	}
}

func TestNowPlayingHidesOtherUsers(t *testing.T) {
	s, _, bobKey := keyFixture(t)
	getWith(t, s.routes(), "/rest/updateNowPlaying.view?id=so-1&playerId=phone", bobKey, true)
	workerKey := newKeyFor(t, s, "worker")
	body := getWith(t, s.routes(), "/rest/getNowPlaying.view", workerKey, true).Body.String()
	entries := subsonicBody(t, body)["nowPlaying"].(map[string]any)["entry"]
	assertIDs(t, "worker's now playing", idsIn(t, entries), nil)
}

// updateNowPlaying with another user's username is refused.
func TestUpdateNowPlayingRefusesOtherUsername(t *testing.T) {
	s, _, bobKey := keyFixture(t)
	body := getWith(t, s.routes(), "/rest/updateNowPlaying.view?id=so-1&username=worker", bobKey, true).Body.String()
	if code := errorCode(t, body); code != 50 {
		t.Fatalf("other username: code %d, want 50: %s", code, body)
	}
}

// #139: one bookmark per user and song; a new one replaces the old; deleting
// touches only the caller's bookmark.
func TestBookmarkCreateReplaceDelete(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/createBookmark.view?id=so-1&position=5000&comment=intro", key, true)
	getWith(t, s.routes(), "/rest/createBookmark.view?id=so-1&position=7000", key, true)

	body := getWith(t, s.routes(), "/rest/getBookmarks.view", key, true).Body.String()
	list := subsonicBody(t, body)["bookmarks"].(map[string]any)["bookmark"].([]any)
	if len(list) != 1 {
		t.Fatalf("bookmarks = %d, want 1 (one per song): %s", len(list), body)
	}
	bm := list[0].(map[string]any)
	if bm["position"] != float64(7000) || bm["comment"] != "" {
		t.Fatalf("bookmark after replace = %v", bm)
	}
	if entry := bm["entry"].(map[string]any); entry["id"] != "so-1" || entry["bookmarkPosition"] != float64(7000) {
		t.Fatalf("bookmark entry = %v", entry)
	}

	getWith(t, s.routes(), "/rest/deleteBookmark.view?id=so-1", key, true)
	body = getWith(t, s.routes(), "/rest/getBookmarks.view", key, true).Body.String()
	if list := subsonicBody(t, body)["bookmarks"].(map[string]any)["bookmark"].([]any); len(list) != 0 {
		t.Fatalf("bookmark after delete: %s", body)
	}
}

// A bookmark is kept when its song leaves the library, but is not returned
// while the song is missing.
func TestBookmarkHidesMissingSong(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/createBookmark.view?id=so-1&position=5000", key, true)
	if _, err := s.Pool.Exec("DELETE FROM songs WHERE id = 'so-1'"); err != nil {
		t.Fatal(err)
	}
	body := getWith(t, s.routes(), "/rest/getBookmarks.view", key, true).Body.String()
	if list := subsonicBody(t, body)["bookmarks"].(map[string]any)["bookmark"].([]any); len(list) != 0 {
		t.Fatalf("bookmark of a missing song returned: %s", body)
	}
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM bookmarks WHERE song_id = 'so-1'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("bookmark row kept = %d, %v; want 1", n, err)
	}
}

func TestBookmarkValidation(t *testing.T) {
	s, _, key := keyFixture(t)
	long := strings.Repeat("x", maxBookmarkComment+1)
	cases := map[string]int{
		"/rest/createBookmark.view?id=so-1&position=-1":                10,
		"/rest/createBookmark.view?id=so-1":                            10,
		"/rest/createBookmark.view?id=so-missing&position=1":           70,
		"/rest/createBookmark.view?id=so-1&position=1&comment=" + long: 10,
	}
	for path, want := range cases {
		body := getWith(t, s.routes(), path, key, true).Body.String()
		if code := errorCode(t, body); code != want {
			t.Errorf("%s: code %d, want %d: %s", path, code, want, body)
		}
	}
}

// #138: the saved queue comes back in order, with the current song, the
// position and who saved it.
func TestPlayQueueSaveAndGet(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/savePlayQueue.view?id=so-1&id=so-2&id=so-3&current=so-2&position=1500&c=TestClient", key, true)

	body := getWith(t, s.routes(), "/rest/getPlayQueue.view", key, true).Body.String()
	pq := subsonicBody(t, body)["playQueue"].(map[string]any)
	if pq["current"] != "so-2" || pq["position"] != float64(1500) || pq["changedBy"] != "TestClient" {
		t.Fatalf("playQueue = %v", pq)
	}
	assertIDs(t, "queue", idsIn(t, pq["entry"]), []string{"so-1", "so-2", "so-3"})
}

// The index form uses the same stored queue, with currentIndex.
func TestPlayQueueByIndex(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/savePlayQueueByIndex.view?id=so-1&id=so-2&id=so-3&currentIndex=2", key, true)
	body := getWith(t, s.routes(), "/rest/getPlayQueueByIndex.view", key, true).Body.String()
	pq := subsonicBody(t, body)["playQueueByIndex"].(map[string]any)
	if pq["currentIndex"] != float64(2) {
		t.Fatalf("currentIndex = %v", pq["currentIndex"])
	}
	// The same queue read by id names its current song.
	body = getWith(t, s.routes(), "/rest/getPlayQueue.view", key, true).Body.String()
	if cur := subsonicBody(t, body)["playQueue"].(map[string]any)["current"]; cur != "so-3" {
		t.Fatalf("current by id = %v, want so-3", cur)
	}
}

// A song that left the library is dropped on read, and the index follows the
// songs that remain.
func TestPlayQueueDropsMissingSongs(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/savePlayQueueByIndex.view?id=so-1&id=so-2&id=so-3&currentIndex=2", key, true)
	if _, err := s.Pool.Exec("DELETE FROM songs WHERE id = 'so-2'"); err != nil {
		t.Fatal(err)
	}
	body := getWith(t, s.routes(), "/rest/getPlayQueueByIndex.view", key, true).Body.String()
	pq := subsonicBody(t, body)["playQueueByIndex"].(map[string]any)
	assertIDs(t, "queue after a song left", idsIn(t, pq["entry"]), []string{"so-1", "so-3"})
	if pq["currentIndex"] != float64(1) {
		t.Fatalf("currentIndex after a song left = %v, want 1", pq["currentIndex"])
	}
}

// Saving an empty queue clears it; a queue must name its current song.
func TestPlayQueueClearAndValidation(t *testing.T) {
	s, _, key := keyFixture(t)
	getWith(t, s.routes(), "/rest/savePlayQueue.view?id=so-1&current=so-1", key, true)
	getWith(t, s.routes(), "/rest/savePlayQueue.view", key, true)
	body := getWith(t, s.routes(), "/rest/getPlayQueue.view", key, true).Body.String()
	if !strings.Contains(body, `"playQueue":{}`) {
		t.Fatalf("cleared queue: %s", body)
	}

	body = getWith(t, s.routes(), "/rest/savePlayQueue.view?id=so-1&current=so-9", key, true).Body.String()
	if code := errorCode(t, body); code != 10 {
		t.Errorf("current not in queue: code %d, want 10", code)
	}
	body = getWith(t, s.routes(), "/rest/savePlayQueueByIndex.view?id=so-1&currentIndex=5", key, true).Body.String()
	if code := errorCode(t, body); code != 10 {
		t.Errorf("index outside queue: code %d, want 10", code)
	}
}
