package analysis

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// workerPlan assigns bpm and beats to folksentia, which runs on workers.
func workerPlan() *Plan {
	return NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {Worker: true, TimeoutDur: time.Minute}},
	}, "")
}

// fingerprint records the file's size and mtime, as a scan does.
func fingerprint(t *testing.T, pool *sql.DB, songID string) Stamp {
	t.Helper()
	var path string
	if err := pool.QueryRow("SELECT path FROM songs WHERE id = ?", songID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := Stamp{Size: info.Size(), MtimeMs: info.ModTime().UnixMilli()}
	if _, err := pool.Exec("UPDATE songs SET file_size = ?, file_mtime_ms = ? WHERE id = ?", st.Size, st.MtimeMs, songID); err != nil {
		t.Fatal(err)
	}
	return st
}

func leaseCount(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM analysis_leases").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWorkerToolsAreNotRunLocally(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good")
	fingerprint(t, pool, "so-good")
	p := workerPlan()
	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("this server selected worker tasks: %d %v", len(tasks), err)
	}
	if reason := p.Available()["folksentia"]; reason != "served by analysis workers" {
		t.Errorf("availability: %q", reason)
	}
	if _, err := p.LeaseWorkerJobs(ctx, pool, "ffmpeg", 1, 5, time.Now()); !errors.Is(err, ErrNotWorkerTool) {
		t.Errorf("lease for a local tool: %v", err)
	}
}

func TestLeaseGivesEachTrackToOneWorker(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good", "other")
	fingerprint(t, pool, "so-good")
	fingerprint(t, pool, "so-other")
	p := workerPlan()
	now := time.Now()

	jobs, err := p.LeaseWorkerJobs(ctx, pool, "folksentia", 7, 1, now)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("first worker: %d %v", len(jobs), err)
	}
	if jobs[0].SongID != "so-good" || len(jobs[0].Features) != 2 || jobs[0].Stamp.Size == 0 {
		t.Errorf("job: %+v", jobs[0])
	}
	// The other track is still free for a second worker.
	jobs, err = p.LeaseWorkerJobs(ctx, pool, "folksentia", 8, 5, now)
	if err != nil || len(jobs) != 1 || jobs[0].SongID != "so-other" {
		t.Fatalf("second worker: %+v %v", jobs, err)
	}
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 9, 5, now); len(jobs) != 0 {
		t.Errorf("a leased track was given out again: %+v", jobs)
	}
	if n := leaseCount(t, pool); n != 2 {
		t.Errorf("leases: %d", n)
	}
}

func TestExpiredLeaseIsGivenAgainAndCounted(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "slow")
	fingerprint(t, pool, "so-slow")
	p := workerPlan()
	now := time.Now()

	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 7, 5, now); len(jobs) != 1 {
		t.Fatal("first lease")
	}
	// The first worker vanished. After the lease, another worker gets the
	// track, and the attempt is counted.
	later := now.Add(WorkerLease + time.Minute)
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 8, 5, later); len(jobs) != 1 {
		t.Fatal("expired lease not given again")
	}
	if n, _ := attemptCount(ctx, pool, "so-slow", "folksentia"); n != 2 {
		t.Fatalf("attempts after two leases: %d", n)
	}
	// Once the attempts are used up, the track is recorded as failed instead.
	much := later.Add(WorkerLease + time.Minute)
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 9, 5, much); len(jobs) != 0 {
		t.Fatalf("a track past its attempts was leased: %+v", jobs)
	}
	errs := analysisErrors(t, pool, "so-slow")
	if len(errs) != 2 || errs[0] != "gave up after 2 interrupted attempts" {
		t.Errorf("stored after giving up: %q", errs)
	}
}

func TestResultNeedsTheLeaseAndTheSameFile(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good")
	st := fingerprint(t, pool, "so-good")
	p := workerPlan()
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 7, 5, time.Now()); len(jobs) != 1 {
		t.Fatal("lease")
	}
	res := WorkerResult{SongID: "so-good", Tool: "folksentia", KeyID: 7, Stamp: st, Output: []byte(folksentiaSample)}

	wrong := res
	wrong.KeyID = 8
	if _, err := p.StoreWorkerResult(ctx, pool, wrong); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("another worker's result: %v", err)
	}

	stale := res
	stale.Stamp.Size++
	if _, err := p.StoreWorkerResult(ctx, pool, stale); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("result for an older file: %v", err)
	}
	if leaseCount(t, pool) != 0 {
		t.Error("the lease of a changed file was kept")
	}
	if _, err := p.StoreWorkerResult(ctx, pool, res); !errors.Is(err, ErrLeaseNotHeld) {
		t.Errorf("result after its lease was released: %v", err)
	}
}

func TestWorkerResultIsStoredLikeALocalRun(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good")
	st := fingerprint(t, pool, "so-good")
	p := workerPlan()
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 7, 5, time.Now()); len(jobs) != 1 {
		t.Fatal("lease")
	}
	changed, err := p.StoreWorkerResult(ctx, pool, WorkerResult{
		SongID: "so-good", Tool: "folksentia", KeyID: 7, Stamp: st, Output: []byte(folksentiaSample)})
	if err != nil || !changed {
		t.Fatalf("store: %v %v", changed, err)
	}
	var source string
	var bpm float64
	if err := pool.QueryRow("SELECT bpm_source, bpm FROM songs WHERE id = 'so-good'").Scan(&source, &bpm); err != nil {
		t.Fatal(err)
	}
	if source != "folksentia" || bpm != 124 {
		t.Errorf("bpm %v from %q", bpm, source)
	}
	if leaseCount(t, pool) != 0 {
		t.Error("the lease was kept after the result")
	}
	if n, _ := attemptCount(ctx, pool, "so-good", "folksentia"); n != 0 {
		t.Errorf("attempts kept after the result: %d", n)
	}
	if errs := analysisErrors(t, pool, "so-good"); len(errs) != 2 || errs[0] != "" {
		t.Errorf("analysis rows: %q", errs)
	}
}

func TestWorkerErrorsAndImpossibleValues(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good", "fast")
	stGood := fingerprint(t, pool, "so-good")
	stFast := fingerprint(t, pool, "so-fast")
	p := workerPlan()
	if jobs, _ := p.LeaseWorkerJobs(ctx, pool, "folksentia", 7, 5, time.Now()); len(jobs) != 2 {
		t.Fatalf("lease: %d", len(jobs))
	}

	if _, err := p.StoreWorkerResult(ctx, pool, WorkerResult{SongID: "so-good", Tool: "folksentia", KeyID: 7,
		Stamp: stGood, Error: "folksentia: no audio decoded"}); err != nil {
		t.Fatal(err)
	}
	if errs := analysisErrors(t, pool, "so-good"); len(errs) != 2 || errs[0] != "folksentia: no audio decoded" {
		t.Errorf("worker error not stored: %q", errs)
	}

	// 500 BPM is no track; the value is refused and the run counts as failed.
	if _, err := p.StoreWorkerResult(ctx, pool, WorkerResult{SongID: "so-fast", Tool: "folksentia", KeyID: 7,
		Stamp: stFast, Output: []byte(`{"schema":1,"file":"x","bpm":500,"bpm_confidence":3}`)}); err != nil {
		t.Fatal(err)
	}
	var bpm sql.NullFloat64
	if err := pool.QueryRow("SELECT bpm FROM songs WHERE id = 'so-fast'").Scan(&bpm); err != nil {
		t.Fatal(err)
	}
	if bpm.Valid {
		t.Errorf("bpm 500 stored: %v", bpm.Float64)
	}
	if errs := analysisErrors(t, pool, "so-fast"); len(errs) != 2 || errs[0] == "" {
		t.Errorf("range error not stored: %q", errs)
	}
}

func TestRunFolksentiaReturnsTheOutput(t *testing.T) {
	dir := t.TempDir()
	command, _ := fakeFolksentia(t, dir)
	src := filepath.Join(dir, "x.flac")
	if err := os.WriteFile(src, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RunFolksentia(context.Background(), command, src, []string{"bpm"}, 0)
	if err != nil || len(out) == 0 {
		t.Fatalf("output: %d bytes, %v", len(out), err)
	}
}
