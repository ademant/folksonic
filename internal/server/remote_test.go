package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// remoteCall makes one request to the remote playback API with an API key.
func remoteCall(t *testing.T, s *State, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// remoteReport posts one state report for a player.
func remoteReport(t *testing.T, s *State, key, pid, status string, applied int64) {
	t.Helper()
	body := fmt.Sprintf(`{"name":"Studio","status":%q,"song_id":"so-1","position_ms":1000,"applied_seq":%d}`, status, applied)
	if rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/"+pid+"/state", key, body); rec.Code != http.StatusOK {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
}

// remoteSend queues a command and returns its sequence number.
func remoteSend(t *testing.T, s *State, key, pid, body string) int64 {
	t.Helper()
	rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/"+pid+"/commands", key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("send %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var out struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Seq
}

// remotePoll asks for the player's commands after a sequence number.
func remotePoll(t *testing.T, s *State, key, pid, query string) []map[string]any {
	t.Helper()
	rec := remoteCall(t, s, http.MethodGet, "/api/remote/players/"+pid+"/commands?"+query, key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Commands []map[string]any `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Commands
}

// A command sent by a controller reaches the player, which applies it and
// acknowledges it, and after that it is not delivered again.
func TestRemoteCommandRoundTrip(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	seq := remoteSend(t, s, key, "studio-1", `{"command":"play"}`)
	cmds := remotePoll(t, s, key, "studio-1", "after=0")
	if len(cmds) != 1 || cmds[0]["command"] != "play" || int64(cmds[0]["seq"].(float64)) != seq {
		t.Fatalf("delivered %v, want the play command %d", cmds, seq)
	}
	if cmds[0]["controller"] != "bob" {
		t.Fatalf("controller = %v, want bob", cmds[0]["controller"])
	}
	remoteReport(t, s, key, "studio-1", "playing", seq)
	if left := remotePoll(t, s, key, "studio-1", "after=0"); len(left) != 0 {
		t.Fatalf("applied command delivered again: %v", left)
	}
}

// The player's applied sequence stops a reconnecting player from replaying
// commands it already applied, even when it asks from zero.
func TestRemoteAppliedSeqStopsReplay(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	first := remoteSend(t, s, key, "studio-1", `{"command":"pause"}`)
	remoteReport(t, s, key, "studio-1", "paused", first)
	remoteSend(t, s, key, "studio-1", `{"command":"next"}`)
	cmds := remotePoll(t, s, key, "studio-1", "after=0")
	if len(cmds) != 1 || cmds[0]["command"] != "next" {
		t.Fatalf("after reconnect: %v, want only next", cmds)
	}
}

// A player that asks for commands waits for them, and gets them as soon as
// they arrive, rather than at the end of the wait.
func TestRemoteLongPollWakesOnCommand(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	go func() {
		time.Sleep(300 * time.Millisecond)
		remoteSend(t, s, key, "studio-1", `{"command":"play"}`)
	}()
	start := time.Now()
	cmds := remotePoll(t, s, key, "studio-1", "after=0&wait=10")
	if len(cmds) != 1 {
		t.Fatalf("long poll returned %v", cmds)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("long poll took %v; it should return when the command arrives", elapsed)
	}
}

// With no commands, a long poll returns an empty list at the end of its wait.
func TestRemoteLongPollTimesOut(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	start := time.Now()
	if cmds := remotePoll(t, s, key, "studio-1", "after=0&wait=1"); len(cmds) != 0 {
		t.Fatalf("expected no commands, got %v", cmds)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("poll returned after %v, before its wait", elapsed)
	}
}

// Two controllers: commands go out in the order they were sent, so the last
// one is what the player ends up doing, and the player reports who it obeyed.
func TestRemoteLastCommandWins(t *testing.T) {
	// Two apps of one user, each with its own key, control the same player.
	s, _, phone := keyFixture(t)
	desk := newKeyFor(t, s, "bob")
	remoteReport(t, s, phone, "studio-1", "playing", 0)
	remoteSend(t, s, phone, "studio-1", `{"command":"pause"}`)
	remoteSend(t, s, desk, "studio-1", `{"command":"play"}`)
	cmds := remotePoll(t, s, phone, "studio-1", "after=0")
	if len(cmds) != 2 || cmds[0]["command"] != "pause" || cmds[1]["command"] != "play" {
		t.Fatalf("order: %v", cmds)
	}
	if cmds[1]["controller"] != "bob" {
		t.Fatalf("last command from %v, want bob", cmds[1]["controller"])
	}
}

// A command older than the TTL is never delivered.
func TestRemoteStaleCommandsAreNotDelivered(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	remoteSend(t, s, key, "studio-1", `{"command":"play"}`)
	old := time.Now().Add(-2 * remoteCommandTTL).UnixMilli()
	if _, err := s.Pool.Exec("UPDATE remote_commands SET created_ms = ?", old); err != nil {
		t.Fatal(err)
	}
	if cmds := remotePoll(t, s, key, "studio-1", "after=0"); len(cmds) != 0 {
		t.Fatalf("stale command delivered: %v", cmds)
	}
}

// Unknown players, unknown commands, bad seeks, bad IDs and bad statuses are refused.
func TestRemoteRefusesBadInput(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"unknown player", http.MethodPost, "/api/remote/players/nobody/commands", `{"command":"play"}`, http.StatusNotFound},
		{"unknown command", http.MethodPost, "/api/remote/players/studio-1/commands", `{"command":"explode"}`, http.StatusBadRequest},
		{"negative seek", http.MethodPost, "/api/remote/players/studio-1/commands", `{"command":"seek","position_ms":-5}`, http.StatusBadRequest},
		{"bad player id", http.MethodPost, "/api/remote/players/bad%20id/commands", `{"command":"play"}`, http.StatusBadRequest},
		{"bad status", http.MethodPost, "/api/remote/players/studio-1/state", `{"status":"flying"}`, http.StatusBadRequest},
		{"bad after", http.MethodGet, "/api/remote/players/studio-1/commands?after=x", "", http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := remoteCall(t, s, c.method, c.path, key, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// Players are per user: another account sees none of them and cannot control them.
func TestRemoteIsPerUser(t *testing.T) {
	s, _, bob := keyFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "carol", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	carol := newKeyFor(t, s, "carol")
	remoteReport(t, s, bob, "studio-1", "playing", 0)
	rec := remoteCall(t, s, http.MethodGet, "/api/remote/players", carol, "")
	if strings.Contains(rec.Body.String(), "studio-1") {
		t.Fatalf("carol sees bob's player: %s", rec.Body.String())
	}
	rec = remoteCall(t, s, http.MethodPost, "/api/remote/players/studio-1/commands", carol, `{"command":"play"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("carol sends to bob's player: %d, want 404", rec.Code)
	}
}

// The applied sequence only moves forward: an older report cannot lower it.
func TestRemoteAppliedSeqNeverGoesBack(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 5)
	remoteReport(t, s, key, "studio-1", "paused", 3)
	rec := remoteCall(t, s, http.MethodGet, "/api/remote/players", key, "")
	if !strings.Contains(rec.Body.String(), `"appliedSeq":5`) {
		t.Fatalf("applied sequence went back: %s", rec.Body.String())
	}
}

// A player may have at most maxPendingRemote commands waiting.
func TestRemotePendingLimit(t *testing.T) {
	s, _, key := keyFixture(t)
	remoteReport(t, s, key, "studio-1", "paused", 0)
	for i := 0; i < maxPendingRemote; i++ {
		remoteSend(t, s, key, "studio-1", `{"command":"play"}`)
	}
	rec := remoteCall(t, s, http.MethodPost, "/api/remote/players/studio-1/commands", key, `{"command":"play"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("command over the limit: %d, want 429", rec.Code)
	}
}
