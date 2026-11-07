package analysis

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/db"
)

// fakeFolksentia writes a stand-in for folksentia: it logs its arguments,
// prints folksentiaSample, an error object for files named broken*, and
// hangs for files named slow*.
func fakeFolksentia(t *testing.T, dir string) (command, argLog string) {
	t.Helper()
	argLog = filepath.Join(dir, "args.log")
	command = filepath.Join(dir, "folksentia")
	script := "#!/bin/sh\necho \"$@\" >> " + argLog + "\n" +
		"for last; do :; done\n" +
		"case \"$(basename \"$last\")\" in\n" +
		"  broken*) echo '{\"schema\":1,\"file\":\"x\",\"error\":\"no audio decoded\"}'; exit 1;;\n" +
		"  slow*) exec sleep 30;;\n" +
		"esac\n" +
		"cat <<'EOF'\n" + folksentiaSample + "\nEOF\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return command, argLog
}

// library creates a database with one song per name; the files exist.
func library(t *testing.T, names ...string) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	pool, err := db.Init(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err := pool.Exec(`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a');
		INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'Al', 'A', 'ar-1')`); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		p := filepath.Join(dir, n+".flac")
		if err := os.WriteFile(p, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, duration_ms)
			VALUES (?, ?, ?, 'A', 'ar-1', 'Al', 'al-1', 'flac', 'audio/flac', 200000)`, "so-"+n, p, n); err != nil {
			t.Fatal(err)
		}
	}
	return pool, dir
}

func folksentiaPlan(command string) *Plan {
	return NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia", Key: "folksentia", Waveform: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {Command: command, TimeoutDur: time.Second}},
	}, "")
}

func TestRunStoresResultsAndErrors(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good", "broken", "slow")
	command, argLog := fakeFolksentia(t, dir)
	p := folksentiaPlan(command)

	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 3 {
		t.Fatalf("pending: %d %v", len(tasks), err)
	}
	cmds, err := p.Commands(tasks)
	if err != nil {
		t.Fatal(err)
	}
	outcome := map[string]Outcome{}
	for _, task := range tasks {
		outcome[task.SongID] = p.RunTask(ctx, pool, cmds, task)
	}
	if o := outcome["so-good"]; !o.Changed || o.Failed {
		t.Fatalf("good: %+v", o)
	}
	if o := outcome["so-broken"]; o.Changed || !o.Failed {
		t.Fatalf("broken: %+v", o)
	}
	if o := outcome["so-slow"]; o.Changed || !o.Failed {
		t.Fatalf("slow: %+v", o)
	}
	args, _ := os.ReadFile(argLog)
	if !strings.Contains(string(args), "--downbeats --waveform=150 -- "+filepath.Join(dir, "good.flac")) {
		t.Fatalf("arguments: %s", args)
	}

	var b, conf float64
	var src string
	var size sql.NullInt64
	if err := pool.QueryRow("SELECT bpm, bpm_source, bpm_confidence, file_size FROM songs WHERE id = 'so-good'").Scan(&b, &src, &conf, &size); err != nil {
		t.Fatal(err)
	}
	if b != 124 || src != "folksentia" || conf != 0.5 || size.Int64 != 5 {
		t.Fatalf("bpm %v %s %v, file_size %v", b, src, conf, size)
	}
	var key string
	if err := pool.QueryRow("SELECT value FROM song_values WHERE song_id = 'so-good' AND name = 'key'").Scan(&key); err != nil || key != `{"tonic":"F#","mode":"minor","confidence":0.81}` {
		t.Fatalf("key %q %v", key, err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM analysis WHERE song_id = 'so-good' AND tool = 'folksentia'
		AND tool_version = 'schema-1' AND error IS NULL`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("analysis rows of good: %d %v", n, err)
	}
	var e1, e2 string
	if err := pool.QueryRow("SELECT error FROM analysis WHERE song_id = 'so-broken' AND feature = 'key'").Scan(&e1); err != nil || !strings.Contains(e1, "no audio decoded") {
		t.Fatalf("broken error %q %v", e1, err)
	}
	if err := pool.QueryRow("SELECT error FROM analysis WHERE song_id = 'so-slow' AND feature = 'key'").Scan(&e2); err != nil || !strings.Contains(e2, "timed out") {
		t.Fatalf("slow error %q %v", e2, err)
	}
	if err := pool.QueryRow("SELECT COUNT(*) FROM song_values WHERE song_id <> 'so-good'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("values stored for failed runs: %d", n)
	}

	// Failures count as results: nothing is pending, so a scan does not
	// retry them. A recalculation runs again, and an unchanged result does
	// not count as a change.
	if tasks, _ := p.Select(ctx, pool, Selection{Pending: true}); len(tasks) != 0 {
		t.Fatalf("still pending: %v", tasks)
	}
	if pf, err := p.PendingFeatures(ctx, pool, "so-good"); err != nil || len(pf) != 0 {
		t.Fatalf("pending features %v %v", pf, err)
	}
	tasks, err = p.Select(ctx, pool, Selection{IDs: []string{"so-good"}, Features: []string{"key"}})
	if err != nil || len(tasks) != 1 || len(tasks[0].Runs) != 1 || strings.Join(tasks[0].Runs[0].Features, ",") != "key" {
		t.Fatalf("recalc selection: %+v %v", tasks, err)
	}
	if o := p.RunTask(ctx, pool, cmds, tasks[0]); o.Changed || o.Failed {
		t.Fatalf("unchanged recalc: %+v", o)
	}

	// A new song is pending until analysed.
	if _, err := pool.Exec("DELETE FROM analysis WHERE song_id = 'so-good' AND feature = 'beats'"); err != nil {
		t.Fatal(err)
	}
	if pf, _ := p.PendingFeatures(ctx, pool, "so-good"); strings.Join(pf, ",") != "beats" {
		t.Fatalf("pending after losing beats: %v", pf)
	}
}

func TestBuiltinNeverReplacesOtherBPM(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "tag", "calc", "none")
	if _, err := pool.Exec(`UPDATE songs SET bpm = 100, bpm_source = 'tag' WHERE id = 'so-tag';
		UPDATE songs SET bpm = 90, bpm_source = 'calc' WHERE id = 'so-calc'`); err != nil {
		t.Fatal(err)
	}
	p := NewPlan(config.Analysis{BPM: config.ToolBuiltin, Auto: true}, "")
	ids := func(sel Selection) string {
		tasks, err := p.Select(ctx, pool, sel)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, t := range tasks {
			s = append(s, t.SongID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(Selection{Pending: true}); got != "so-none" {
		t.Errorf("pending: %s", got)
	}
	if got := ids(Selection{}); got != "so-calc,so-none" {
		t.Errorf("recalc: %s", got)
	}
	if got := ids(Selection{MissingBPM: true, Features: []string{"bpm"}}); got != "so-none" {
		t.Errorf("missing: %s", got)
	}
	// Even if asked to store over a tag, the estimator's value is dropped.
	tx := Run{Group: p.Groups[0], Features: []string{"bpm"}}
	if changed, err := store(ctx, pool, "so-tag", tx, "1", Values{"bpm": &BPM{Value: 50}}, nil, nil); err != nil || changed {
		t.Fatalf("store over tag: %v %v", changed, err)
	}
	if _, err := store(ctx, pool, "so-gone", tx, "1", Values{}, nil, nil); !errors.Is(err, errSongGone) {
		t.Fatalf("removed song: %v", err)
	}
}

func TestCommandsReportsMissingTool(t *testing.T) {
	pool, _ := library(t, "x")
	p := folksentiaPlan("/nonexistent/folksentia")
	tasks, err := p.Select(context.Background(), pool, Selection{Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Commands(tasks); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing tool: %v", err)
	}
	if r := p.Available()["folksentia"]; !strings.Contains(r, "not an executable") {
		t.Fatalf("available: %q", r)
	}
}
