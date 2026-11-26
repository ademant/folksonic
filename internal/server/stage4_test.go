package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// callAny is call without the check for status ok: it returns the envelope.
func callAny(t *testing.T, s *State, endpoint string, q url.Values) map[string]any {
	t.Helper()
	q.Set("u", "admin")
	q.Set("p", "s3cret")
	req := httptest.NewRequest(http.MethodGet, "/rest/"+endpoint+"?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var env map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: %v: %s", endpoint, err, rec.Body.String())
	}
	return env["subsonic-response"]
}

func envError(r map[string]any) float64 {
	e, _ := r["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

// Playlist updates only touch the owner's playlists, and a delete takes the
// songs with it (#184).
func TestPlaylistWritesAreOwnerScoped(t *testing.T) {
	s := pagingFixture(t)
	ctx := context.Background()
	admin := userIDOf(t, s, "admin")
	if err := repoCreatePlaylist(ctx, s.Pool, admin, "pl-1", "Mine", nowISO8601()); err != nil {
		t.Fatal(err)
	}
	if err := repoAppendPlaylistSongs(ctx, s.Pool, "pl-1", []string{"so-1", "so-2"}); err != nil {
		t.Fatal(err)
	}
	if err := repoRenamePlaylist(ctx, s.Pool, admin+1, "pl-1", "Stolen", nowISO8601()); err != nil {
		t.Fatal(err)
	}
	if err := repoSetPlaylistComment(ctx, s.Pool, admin+1, "pl-1", "x", nowISO8601()); err != nil {
		t.Fatal(err)
	}
	pl, err := repoFindPlaylist(ctx, s.Pool, admin, "pl-1")
	if err != nil || pl == nil || pl.Name != "Mine" || pl.Comment.Valid {
		t.Fatalf("another owner changed the playlist: %+v %v", pl, err)
	}
	if err := repoDeletePlaylist(ctx, s.Pool, admin, "pl-1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM playlist_songs WHERE playlist_id = 'pl-1'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d songs left behind: %v", n, err)
	}
}

// Only existing items can be starred; unstarring an unknown id still works
// (#185).
func TestStarNeedsAnExistingTarget(t *testing.T) {
	s := pagingFixture(t)
	for _, q := range []url.Values{{"id": {"so-404"}}, {"albumId": {"al-404"}}, {"artistId": {"nope"}}} {
		if r := callAny(t, s, "star", q); envError(r) != 70 {
			t.Fatalf("star %v: %v", q, r)
		}
	}
	for _, q := range []url.Values{{"id": {"so-1"}}, {"albumId": {"al-1"}}, {"artistId": {"ar-1"}}} {
		call(t, s, http.MethodGet, "star", q, nil)
	}
	call(t, s, http.MethodGet, "unstar", url.Values{"id": {"so-404"}}, nil)
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM starred").Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d starred rows: %v", n, err)
	}
}

// Scrobble times far ahead become now; past ones are kept for a year (#185).
func TestClampPlayTime(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC).UnixMilli()
	day := int64(24 * time.Hour / time.Millisecond)
	for _, c := range []struct{ in, want int64 }{
		{now, now},
		{now + 4*60*1000, now + 4*60*1000},
		{now + 10*60*1000, now},
		{now + 3650*day, now},
		{now - 30*day, now - 30*day},
		{now - 400*day, now - 365*day},
	} {
		if got := clampPlayTime(c.in, now); got != c.want {
			t.Errorf("clamp(%d) = %d, want %d", c.in-now, got-now, c.want-now)
		}
	}
	s := pagingFixture(t)
	future := time.Now().Add(48 * time.Hour).UnixMilli()
	call(t, s, http.MethodGet, "scrobble", url.Values{"id": {"so-1"}, "time": {fmt.Sprint(future)}}, nil)
	var last int64
	if err := s.Pool.QueryRow("SELECT last_played_ms FROM play_counts WHERE song_id = 'so-1'").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last > time.Now().UnixMilli() {
		t.Fatalf("a future play time was stored: %d", last)
	}
}

// The now-playing store bounds player ids and players per user (#185).
func TestNowPlayingIsBounded(t *testing.T) {
	s := pagingFixture(t)
	long := strings.Repeat("p", maxPlayerIDLen+1)
	if r := callAny(t, s, "updateNowPlaying", url.Values{"id": {"so-1"}, "playerId": {long}}); envError(r) != 10 {
		t.Fatalf("long player id: %v", r)
	}
	var n nowPlayingStore
	start := time.Now()
	for i := range maxNowPlayingPerUser + 5 {
		n.set(1, fmt.Sprint("p", i), "so-1", start.Add(time.Duration(i)*time.Second))
	}
	list := n.list(1, start.Add(time.Minute))
	if len(list) != maxNowPlayingPerUser {
		t.Fatalf("%d players kept", len(list))
	}
	for _, e := range list {
		if e.playerID == "p0" {
			t.Fatal("the oldest player was kept")
		}
	}
}

// An account keeps at most maxRemotePlayers players: an offline one makes
// room, an online one does not; long fields are refused (#169).
func TestRemotePlayersAreBounded(t *testing.T) {
	s, _, key := keyFixture(t)
	for i := range maxRemotePlayers {
		remoteReport(t, s, key, fmt.Sprint("player-", i), "playing", 0)
	}
	body := `{"status":"playing"}`
	if rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/one-more/state", key, body); rec.Code != http.StatusConflict ||
		decodeMap(t, rec)["error"] != codeTooManyPlayers {
		t.Fatalf("17th online player: %d %s", rec.Code, rec.Body.String())
	}
	// player-0 goes quiet; the new player replaces it.
	old := time.Now().Add(-2 * remoteOnlineWindow).UnixMilli()
	if _, err := s.Pool.Exec("UPDATE remote_players SET reported_ms = ? WHERE player_id = 'player-0'", old); err != nil {
		t.Fatal(err)
	}
	remoteReport(t, s, key, "one-more", "playing", 0)
	var n, gone int
	_ = s.Pool.QueryRow("SELECT COUNT(*), COUNT(*) FILTER (WHERE player_id = 'player-0') FROM remote_players").Scan(&n, &gone)
	if n != maxRemotePlayers || gone != 0 {
		t.Fatalf("%d players, player-0 kept: %d", n, gone)
	}
	long := strings.Repeat("x", maxRemoteField+1)
	for _, b := range []string{`{"status":"playing","song_id":"` + long + `"}`, `{"status":"playing","controller":"` + long + `"}`} {
		if rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/player-1/state", key, b); rec.Code != http.StatusBadRequest {
			t.Fatalf("long field: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/player-1/commands", key, `{"command":"play","song_id":"`+long+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("long command song_id: %d", rec.Code)
	}
}

// An eviction stops a preload or lookup that read before it from storing
// the old path (#197).
func TestSongCacheEvictionWinsOverOlderReads(t *testing.T) {
	s := pagingFixture(t)
	gen := s.songCacheGen.Load()
	s.evictSong("so-1")
	if s.cacheSong(gen, "so-1", songRef{Path: "/old/path.mp3"}) {
		t.Fatal("a read from before the eviction was cached")
	}
	if _, ok := s.songCache.Load("so-1"); ok {
		t.Fatal("stale entry in the cache")
	}
	path, _, found, err := s.lookupSong(context.Background(), "so-1")
	if err != nil || !found || path != "/m/1.mp3" {
		t.Fatalf("lookup: %q %v %v", path, found, err)
	}
	if v, ok := s.songCache.Load("so-1"); !ok || v.(songRef).Path != "/m/1.mp3" {
		t.Fatal("a fresh lookup was not cached")
	}
	// A preload that starts after the eviction fills the cache.
	s.preloadSongs(context.Background())
	if _, ok := s.songCache.Load("so-5"); !ok {
		t.Fatal("preload did not fill the cache")
	}
}

// Starred lists, random songs and playlist totals, now each one query, give
// the same answers (#200).
func TestListEndpointsWithoutPerRowQueries(t *testing.T) {
	s := pagingFixture(t)
	ctx := context.Background()
	admin := userIDOf(t, s, "admin")
	for i, id := range []string{"so-1", "so-4", "al-1", "ar-3"} {
		if err := repoStar(ctx, s.Pool, admin, id, fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	songs, err := repoStarredSongs(ctx, s.Pool, admin)
	if err != nil || len(songs) != 2 || songs[0].Song.ID != "so-4" || songs[0].When != "2026-01-02T00:00:00Z" || songs[1].Song.Title != "Same" {
		t.Fatalf("starred songs: %+v %v", songs, err)
	}
	albums, err := repoStarredAlbums(ctx, s.Pool, admin)
	if err != nil || len(albums) != 1 || albums[0].Album.SongCount != 2 || albums[0].Album.DurationMs != 2000 {
		t.Fatalf("starred albums: %+v %v", albums, err)
	}
	artists, err := repoStarredArtists(ctx, s.Pool, admin)
	if err != nil || len(artists) != 1 || artists[0].Artist.Name != "Charlie" {
		t.Fatalf("starred artists: %+v %v", artists, err)
	}

	random, err := repoRandomSongs(ctx, s.Pool, 3, nil, nil, "folk")
	if err != nil || len(random) != 3 {
		t.Fatalf("random: %d %v", len(random), err)
	}
	seen := map[string]bool{}
	for _, sg := range random {
		if seen[sg.ID] || sg.Path == "" {
			t.Fatalf("random songs: %+v", random)
		}
		seen[sg.ID] = true
	}

	if err := repoCreatePlaylist(ctx, s.Pool, admin, "pl-1", "A", nowISO8601()); err != nil {
		t.Fatal(err)
	}
	if err := repoAppendPlaylistSongs(ctx, s.Pool, "pl-1", []string{"so-1", "so-2", "so-404"}); err != nil {
		t.Fatal(err)
	}
	r := call(t, s, http.MethodGet, "getPlaylists", url.Values{}, nil)
	pl := r.data["playlists"].(map[string]any)["playlist"].([]any)[0].(map[string]any)
	if pl["songCount"] != float64(2) || pl["duration"] != float64(2) {
		t.Fatalf("playlist totals: %v", pl)
	}

	counts, err := repoAlbumCountsByArtist(ctx, s.Pool, []string{"ar-1", "ar-2"})
	if err != nil || counts["ar-1"] != 2 || counts["ar-2"] != 0 || counts["ar-3"] != 0 {
		t.Fatalf("album counts: %v %v", counts, err)
	}
}
