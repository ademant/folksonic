package analysis

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// seedAttempts sets the interrupted-attempt count of one song and tool.
func seedAttempts(t *testing.T, pool *sql.DB, songID, tool string, n int) {
	t.Helper()
	if err := setAttemptCount(context.Background(), pool, songID, tool, n); err != nil {
		t.Fatal(err)
	}
}

func analysisErrors(t *testing.T, pool *sql.DB, songID string) []string {
	t.Helper()
	rows, err := pool.Query("SELECT COALESCE(error, '') FROM analysis WHERE song_id = ?", songID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestTrackIsGivenUpAfterInterruptedAttempts(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good")
	command, argLog := fakeFolksentia(t, dir)
	p := folksentiaPlan(command)
	seedAttempts(t, pool, "so-good", "folksentia", maxAttempts)

	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pending: %d %v", len(tasks), err)
	}
	cmds, _ := p.Commands(tasks)
	out := p.RunTask(ctx, pool, cmds, tasks[0])

	if _, err := os.Stat(argLog); err == nil {
		t.Fatal("the tool ran for a track that was given up on")
	}
	if !out.Failed {
		t.Error("a given-up track is reported as failed")
	}
	for _, e := range analysisErrors(t, pool, "so-good") {
		if e != "gave up after 2 interrupted attempts" {
			t.Errorf("stored error %q", e)
		}
	}
	if n, _ := attemptCount(ctx, pool, "so-good", "folksentia"); n != 0 {
		t.Errorf("attempts after giving up: %d", n)
	}
	// Recorded as a result, so the next automatic pass does not select it.
	left, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(left) != 0 {
		t.Fatalf("given-up track still pending: %d %v", len(left), err)
	}
}

func TestManualRecalcStartsGivenUpTrackAfresh(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good")
	command, argLog := fakeFolksentia(t, dir)
	p := folksentiaPlan(command)
	seedAttempts(t, pool, "so-good", "folksentia", maxAttempts)

	tasks, err := p.Select(ctx, pool, Selection{IDs: []string{"so-good"}})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("recalc select: %d %v", len(tasks), err)
	}
	if err := p.ResetAttempts(ctx, pool, tasks); err != nil {
		t.Fatal(err)
	}
	cmds, _ := p.Commands(tasks)
	if out := p.RunTask(ctx, pool, cmds, tasks[0]); out.Failed {
		t.Fatal("recalc failed")
	}
	if _, err := os.Stat(argLog); err != nil {
		t.Fatal("the tool did not run after the reset")
	}
	if n, _ := attemptCount(ctx, pool, "so-good", "folksentia"); n != 0 {
		t.Errorf("attempts after a stored result: %d", n)
	}
}

func TestAttemptIsCountedWhileRunningAndUndoneOnStop(t *testing.T) {
	pool, dir := library(t, "slow")
	command, argLog := fakeFolksentia(t, dir)
	p := folksentiaPlan(command)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pending: %d %v", len(tasks), err)
	}
	cmds, _ := p.Commands(tasks)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunTask(ctx, pool, cmds, tasks[0])
	}()
	waitFor(t, func() bool { _, err := os.Stat(argLog); return err == nil })
	// The count is written before the tool starts, so a folksonic that dies
	// now leaves it behind for the next start.
	if n, err := attemptCount(context.Background(), pool, "so-slow", "folksentia"); err != nil || n != 1 {
		t.Fatalf("while running: attempts %d, %v", n, err)
	}
	cancel()
	<-done
	// A stop is not an interrupted attempt, so the count goes back.
	if n, _ := attemptCount(context.Background(), pool, "so-slow", "folksentia"); n != 0 {
		t.Errorf("after stop: attempts %d, want 0", n)
	}
}

func TestMaxDurationSkipsLongTracks(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "long")
	command, argLog := fakeFolksentia(t, dir)
	p := NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {
			Command: command, TimeoutDur: time.Second, MaxDurationDur: 100 * time.Second,
		}},
	}, "")
	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("pending: %d %v", len(tasks), err)
	}
	cmds, _ := p.Commands(tasks)
	if out := p.RunTask(ctx, pool, cmds, tasks[0]); out.Failed {
		t.Error("a skipped track is not a failure")
	}
	if _, err := os.Stat(argLog); err == nil {
		t.Fatal("the tool ran on a track over max_duration")
	}
	errs := analysisErrors(t, pool, "so-long")
	if len(errs) != 2 || errs[0] != "longer than max_duration 1m40s" {
		t.Errorf("stored reasons: %q", errs)
	}
	left, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(left) != 0 {
		t.Fatalf("skipped track still pending: %d %v", len(left), err)
	}
}

// toolScript writes an executable shell script and returns its path.
func toolScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestToolChildIsFirstOOMVictim(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("oom_score_adj is Linux only")
	}
	out := filepath.Join(t.TempDir(), "score")
	// Give the parent time to set the score; the tool reads it itself.
	script := toolScript(t, "sleep 0.5; cat /proc/$$/oom_score_adj > "+out)
	if _, _, err := runTool(context.Background(), script, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "1000" {
		t.Errorf("oom_score_adj = %q, want 1000", got)
	}
}

func TestMemoryLimitCapsTheTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RLIMIT_DATA is Linux only")
	}
	script := toolScript(t, "sleep 0.5; ulimit -d")
	out, _, err := runTool(context.Background(), script, nil, nil, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	// ulimit reports kibibytes.
	if got := strings.TrimSpace(string(out)); got != "65536" {
		t.Errorf("data limit = %q KiB, want 65536", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the condition")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
