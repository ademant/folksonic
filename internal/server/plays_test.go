package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// songBody returns getSong for id as the caller of key sees it.
func songBody(t *testing.T, s *State, key, id string) string {
	t.Helper()
	return getWith(t, s.routes(), "/rest/getSong.view?id="+id, key, true).Body.String()
}

// scrobble submits one play (or a now-playing report) and reports the status.
func scrobble(t *testing.T, s *State, key, query string) string {
	t.Helper()
	return getWith(t, s.routes(), "/rest/scrobble.view?"+query, key, true).Body.String()
}

// #136: each submission counts one play for the caller and the song.
func TestScrobbleCountsEachPlay(t *testing.T) {
	s, _, key := keyFixture(t)
	for i := 0; i < 2; i++ {
		// Distinct times: plays of one song in the same millisecond are one
		// play (the dedupe key), which made this test flaky.
		if body := scrobble(t, s, key, fmt.Sprintf("id=so-1&time=%d", 1000*(i+1))); !strings.Contains(body, `"status":"ok"`) {
			t.Fatalf("scrobble: %s", body)
		}
	}
	body := songBody(t, s, key, "so-1")
	if !strings.Contains(body, `"playCount":2`) {
		t.Fatalf("getSong after two plays: %s", body)
	}
	if !strings.Contains(body, `"played":"`) {
		t.Fatalf("getSong after plays has no played: %s", body)
	}
}

// A retried submission carries the same play time, so it counts once, also
// when the time is clamped (more than a year old or in the future, #185).
func TestScrobbleRetryWithSameTimeCountsOnce(t *testing.T) {
	s, _, key := keyFixture(t)
	for _, played := range []string{"1700000000000", fmt.Sprint(time.Now().Add(48 * time.Hour).UnixMilli())} {
		for i := 0; i < 3; i++ {
			scrobble(t, s, key, "id=so-1&time="+played)
		}
	}
	for i := 0; i < 3; i++ {
		scrobble(t, s, key, "id=so-2&time=1700000000000")
	}
	if body := songBody(t, s, key, "so-2"); !strings.Contains(body, `"playCount":1`) {
		t.Fatalf("retried play counted more than once: %s", body)
	}
	if body := songBody(t, s, key, "so-1"); !strings.Contains(body, `"playCount":2`) {
		t.Fatalf("retried plays (two distinct times) counted wrong: %s", body)
	}
}

// The played field is the time of the last play, in UTC.
func TestScrobbleSetsPlayedFromTime(t *testing.T) {
	s, _, key := keyFixture(t)
	played := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second).UTC()
	scrobble(t, s, key, fmt.Sprintf("id=so-1&time=%d", played.UnixMilli()))
	if body := songBody(t, s, key, "so-1"); !strings.Contains(body, `"played":"`+played.Format(time.RFC3339)+`"`) {
		t.Fatalf("played: %s", body)
	}
}

// submission=false is now playing: accepted, not counted.
func TestNowPlayingIsNotCounted(t *testing.T) {
	s, _, key := keyFixture(t)
	if body := scrobble(t, s, key, "id=so-1&submission=false"); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("now playing: %s", body)
	}
	body := songBody(t, s, key, "so-1")
	if !strings.Contains(body, `"playCount":0`) || strings.Contains(body, `"played"`) {
		t.Fatalf("now playing was counted: %s", body)
	}
}

// Counts belong to the user who played, not to the library.
func TestPlayCountsArePerUser(t *testing.T) {
	s, _, bobKey := keyFixture(t)
	workerKey, err := users.CreateAPIKey(context.Background(), s.Pool, userIDOf(t, s, "worker"), "desk", 0)
	if err != nil {
		t.Fatal(err)
	}
	scrobble(t, s, bobKey, "id=so-1&time=1000")
	scrobble(t, s, bobKey, "id=so-1&time=2000")
	if body := songBody(t, s, workerKey, "so-1"); !strings.Contains(body, `"playCount":0`) {
		t.Fatalf("worker sees bob's plays: %s", body)
	}
	if body := songBody(t, s, bobKey, "so-1"); !strings.Contains(body, `"playCount":2`) {
		t.Fatalf("bob's count: %s", body)
	}
}

// A song that is not in the library is refused, and no play of the request
// is counted.
func TestScrobbleUnknownSongRecordsNothing(t *testing.T) {
	s, _, key := keyFixture(t)
	body := scrobble(t, s, key, "id=so-1&id=so-missing")
	if !strings.Contains(body, `"code":70`) {
		t.Fatalf("unknown song: %s", body)
	}
	if got := songBody(t, s, key, "so-1"); !strings.Contains(got, `"playCount":0`) {
		t.Fatalf("a play was counted with a bad id in the same request: %s", got)
	}
}

// Without an id the request is malformed.
func TestScrobbleNeedsAnID(t *testing.T) {
	s, _, key := keyFixture(t)
	if body := scrobble(t, s, key, ""); !strings.Contains(body, `"code":10`) {
		t.Fatalf("no id: %s", body)
	}
}

// Keys older than the retry window are removed; recent ones stay.
func TestPrunePlayKeysDropsExpiredKeys(t *testing.T) {
	s, _, _ := keyFixture(t)
	ctx := context.Background()
	now := "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"
	for _, q := range []string{
		"INSERT INTO play_keys (user_id, song_id, played_ms, seen_ms) VALUES (1, 'so-1', 1, " + now + " - 172800000)", // two days ago
		"INSERT INTO play_keys (user_id, song_id, played_ms, seen_ms) VALUES (1, 'so-2', 2, " + now + ")",
	} {
		if _, err := s.Pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrunePlayKeys(ctx, s.Pool); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM play_keys").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("play_keys after prune = %d, want 1 (the recent key)", n)
	}
}
