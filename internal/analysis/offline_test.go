package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// offlinePlan assigns bpm and beats to a folksentia without a command, which
// the server never runs (#105).
func offlinePlan() *Plan {
	return NewPlan(config.Analysis{
		BPM: "folksentia", Beats: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {TimeoutDur: time.Minute}},
	}, "")
}

// exportAll exports every folksentia track of the library in dir.
func exportAll(t *testing.T, pool *sql.DB, dir string) *JobsFile {
	t.Helper()
	f, _, err := offlinePlan().ExportJobs(context.Background(), pool, Selection{}, []string{dir}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// resultsFile writes lines to a results file and returns its path.
func resultsFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// line is a result line for job with result (raw JSON).
func line(t *testing.T, job OfflineJob, result string) string {
	t.Helper()
	j, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	return `{"job":` + string(j) + `,"local":{"size":` + jsonInt(job.Size) + `,"mtimeMs":` + jsonInt(job.MtimeMs) +
		`},"toolVersion":"folksentia 0.5.0","result":` + strings.ReplaceAll(result, "\n", "") + `}`
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func songBPM(t *testing.T, pool *sql.DB, id string) (sql.NullFloat64, sql.NullString) {
	t.Helper()
	var bpm sql.NullFloat64
	var source sql.NullString
	if err := pool.QueryRow("SELECT bpm, bpm_source FROM songs WHERE id = ?", id).Scan(&bpm, &source); err != nil {
		t.Fatal(err)
	}
	return bpm, source
}

func TestOfflineToolIsNotRunHere(t *testing.T) {
	ctx := context.Background()
	pool, _ := library(t, "good")
	p := offlinePlan()
	tasks, err := p.Select(ctx, pool, Selection{Pending: true})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("this server selected offline tasks: %d %v", len(tasks), err)
	}
	if reason := p.Available()["folksentia"]; reason != OfflineOnly {
		t.Errorf("availability: %q", reason)
	}
	if tasks, _ := p.Select(ctx, pool, Selection{Tool: OfflineTool, Pending: true}); len(tasks) != 1 {
		t.Errorf("an export selects %d tasks, want 1", len(tasks))
	}
	// A folksentia with a command is run here, and can be exported as well.
	local := folksentiaPlan("/usr/local/bin/folksentia")
	if tasks, _ := local.Select(ctx, pool, Selection{Pending: true}); len(tasks) != 1 {
		t.Errorf("local folksentia: %d tasks", len(tasks))
	}
	if _, _, err := NewPlan(config.Analysis{BPM: config.ToolBuiltin}, "").ExportJobs(ctx, pool, Selection{}, nil, time.Now()); err != ErrNoOfflineTool {
		t.Errorf("export without folksentia: %v", err)
	}
}

func TestExportJobs(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good", "gone", "elsewhere")
	if err := os.Remove(filepath.Join(dir, "gone.flac")); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, err := pool.Exec("UPDATE songs SET path = ? WHERE id = 'so-elsewhere'", filepath.Join(other, "x.flac")); err != nil {
		t.Fatal(err)
	}
	f, rep, err := offlinePlan().ExportJobs(ctx, pool, Selection{}, []string{dir}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.Format != JobsFormat || f.Version != 1 || f.Tool != "folksentia" || f.Schema != 1 {
		t.Errorf("header: %+v", f)
	}
	if !f.Options.Downbeats || f.Options.Waveform != 0 || f.Options.Fingerprint || f.Options.Onsets {
		t.Errorf("options for bpm+beats: %+v", f.Options)
	}
	if o := folksentiaOptions([]string{"onsets", "rhythm", "spectral"}); o != (FolksentiaOptions{Onsets: true}) {
		t.Errorf("options for onsets+rhythm+spectral: %+v", o)
	}
	if rep.Jobs != 1 || rep.Unreadable != 1 || rep.Outside != 1 || len(f.Jobs) != 1 {
		t.Fatalf("report %+v, jobs %+v", rep, f.Jobs)
	}
	j := f.Jobs[0]
	if j.ID != "so-good" || j.Path != "good.flac" || j.Size != 5 || j.MtimeMs == 0 || j.DurationMs != 200000 ||
		strings.Join(j.Features, ",") != "bpm,beats" {
		t.Errorf("job: %+v", j)
	}

	// Nested folders: the path is relative to the innermost one.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if rel, ok := RelativePath([]string{dir, sub}, filepath.Join(sub, "a", "b.flac")); !ok || rel != "a/b.flac" {
		t.Errorf("nested: %q %v", rel, ok)
	}
	if _, ok := RelativePath([]string{sub}, filepath.Join(dir, "good.flac")); ok {
		t.Error("a path outside the folder was made relative")
	}

	// -features waveform is not folksentia's here: nothing is selected for it.
	f, _, err = offlinePlan().ExportJobs(ctx, pool, Selection{Features: []string{"bpm"}, IDs: []string{"so-good"}}, []string{dir}, time.Now())
	if err != nil || len(f.Jobs) != 1 || strings.Join(f.Jobs[0].Features, ",") != "bpm" || f.Options.Downbeats {
		t.Errorf("bpm only: %+v %v", f, err)
	}
}

func TestImportStoresLikeALocalRun(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good")
	f := exportAll(t, pool, dir)
	p := offlinePlan()
	path := resultsFile(t, line(t, f.Jobs[0], folksentiaSample))

	rep, err := p.ImportResults(ctx, pool, path, ImportOptions{DryRun: true})
	if err != nil || rep.Stored != 1 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if bpm, _ := songBPM(t, pool, "so-good"); bpm.Valid {
		t.Fatal("a dry run stored a value")
	}

	rep, err = p.ImportResults(ctx, pool, path, ImportOptions{})
	if err != nil || rep.Lines != 1 || rep.Stored != 1 || rep.Changed != 1 || rep.Failed != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("import: %+v %v", rep, err)
	}
	if bpm, source := songBPM(t, pool, "so-good"); bpm.Float64 != 124 || source.String != "folksentia" {
		t.Errorf("bpm %v from %q", bpm, source.String)
	}
	if errs := analysisErrors(t, pool, "so-good"); len(errs) != 2 || errs[0] != "" || errs[1] != "" {
		t.Errorf("analysis rows: %q", errs)
	}
	var version string
	if err := pool.QueryRow("SELECT tool_version FROM analysis WHERE song_id = 'so-good' AND feature = 'bpm'").Scan(&version); err != nil || version != "folksentia 0.5.0" {
		t.Errorf("tool version %q %v", version, err)
	}
	// The imported song no longer counts as pending.
	if tasks, _ := p.Select(ctx, pool, Selection{Tool: OfflineTool, Pending: true}); len(tasks) != 0 {
		t.Errorf("still pending after the import: %d", len(tasks))
	}

	rep, err = p.ImportResults(ctx, pool, path, ImportOptions{})
	if err != nil || rep.Stored != 1 || rep.Changed != 0 {
		t.Errorf("second import: %+v %v", rep, err)
	}
}

func TestImportSkipsAndRefusesLines(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good", "fast", "moved", "fails", "twice", "wrongschema", "desktopgone")
	f := exportAll(t, pool, dir)
	jobs := map[string]OfflineJob{}
	for _, j := range f.Jobs {
		jobs[j.ID] = j
	}
	// so-moved changes on the server after the export (a newer mtime).
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "moved.flac"), later, later); err != nil {
		t.Fatal(err)
	}
	unknown := jobs["so-good"]
	unknown.ID = "so-nosuchsong"
	twiceOld := line(t, jobs["so-twice"], `{"schema":1,"file":"x","bpm":90}`)
	twiceNew := line(t, jobs["so-twice"], `{"schema":1,"file":"x","bpm":100}`)
	desktopGone, _ := json.Marshal(map[string]any{"job": jobs["so-desktopgone"], "skipped": "missing"})

	path := resultsFile(t,
		line(t, jobs["so-good"], folksentiaSample),
		line(t, jobs["so-fast"], `{"schema":1,"file":"x","bpm":500,"bpm_confidence":3}`),
		`{"job": not json`,
		line(t, jobs["so-moved"], folksentiaSample),
		line(t, jobs["so-fails"], `{"schema":1,"file":"x","error":"no audio decoded"}`),
		twiceOld,
		line(t, unknown, folksentiaSample),
		line(t, jobs["so-wrongschema"], `{"schema":2,"file":"x","bpm":120}`),
		string(desktopGone),
		`{"result":{"schema":1}}`,
		twiceNew,
	)
	rep, err := offlinePlan().ImportResults(ctx, pool, path, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"invalid": 3, "stale": 1, "unknown": 1, "duplicate": 1, "missing": 1}
	for k, n := range want {
		if rep.Skipped[k] != n {
			t.Errorf("skipped %s: %d, want %d (%v)", k, rep.Skipped[k], n, rep.Skipped)
		}
	}
	if rep.Lines != 11 || rep.Stored != 4 || rep.Failed != 2 || len(rep.Problems) != 3 {
		t.Errorf("report: %+v", rep)
	}

	if bpm, _ := songBPM(t, pool, "so-good"); bpm.Float64 != 124 {
		t.Errorf("good: %v", bpm)
	}
	if bpm, _ := songBPM(t, pool, "so-fast"); bpm.Valid {
		t.Errorf("bpm 500 stored: %v", bpm.Float64)
	}
	if errs := analysisErrors(t, pool, "so-fast"); len(errs) != 2 || !strings.Contains(errs[0], "outside 20–300") {
		t.Errorf("range error not stored: %q", errs)
	}
	if errs := analysisErrors(t, pool, "so-fails"); len(errs) != 2 || !strings.Contains(errs[0], "no audio decoded") {
		t.Errorf("folksentia error not stored: %q", errs)
	}
	if bpm, _ := songBPM(t, pool, "so-twice"); bpm.Float64 != 100 {
		t.Errorf("the last line of a song should win: %v", bpm)
	}
	for _, id := range []string{"so-moved", "so-wrongschema", "so-desktopgone"} {
		if bpm, _ := songBPM(t, pool, id); bpm.Valid {
			t.Errorf("%s stored: %v", id, bpm.Float64)
		}
	}

	// -size-only accepts the file whose mtime changed.
	path = resultsFile(t, line(t, jobs["so-moved"], folksentiaSample))
	if rep, err := offlinePlan().ImportResults(ctx, pool, path, ImportOptions{SizeOnly: true}); err != nil || rep.Stored != 1 {
		t.Errorf("size only: %+v %v", rep, err)
	}
}

// Features no longer assigned to folksentia are not stored from an old file.
func TestImportStoresOnlyAssignedFeatures(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good")
	f := exportAll(t, pool, dir)
	beatsOnly := NewPlan(config.Analysis{
		BPM: config.ToolBuiltin, Beats: "folksentia", Auto: true,
		Tools: map[string]config.AnalysisTool{"folksentia": {TimeoutDur: time.Minute}},
	}, "")
	rep, err := beatsOnly.ImportResults(ctx, pool, resultsFile(t, line(t, f.Jobs[0], folksentiaSample)), ImportOptions{})
	if err != nil || rep.Stored != 1 {
		t.Fatalf("import: %+v %v", rep, err)
	}
	if bpm, _ := songBPM(t, pool, "so-good"); bpm.Valid {
		t.Errorf("bpm stored although it is the builtin's now: %v", bpm.Float64)
	}
	if errs := analysisErrors(t, pool, "so-good"); len(errs) != 1 {
		t.Errorf("analysis rows: %q", errs)
	}
}

// A result line longer than the reader's buffer is read whole.
func TestImportReadsLongLines(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "good")
	f := exportAll(t, pool, dir)
	beats := make([]string, 300000)
	for i := range beats {
		beats[i] = "0.5"
	}
	result := `{"schema":1,"file":"x","bpm":120,"beats":[` + strings.Join(beats, ",") + `],"downbeatIndexes":[0]}`
	rep, err := offlinePlan().ImportResults(ctx, pool, resultsFile(t, line(t, f.Jobs[0], result)), ImportOptions{})
	if err != nil || rep.Stored != 1 || rep.Skipped["invalid"] != 0 {
		t.Fatalf("long line: %+v %v", rep, err)
	}
}

// A dry run reports the same changes as the import it previews, and stores
// nothing (#119).
func TestImportDryRunReportsChanges(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "same", "new", "other")
	f := exportAll(t, pool, dir)
	jobs := map[string]OfflineJob{}
	for _, j := range f.Jobs {
		jobs[j.ID] = j
	}
	p := offlinePlan()
	// so-same and so-other already hold this result.
	first := resultsFile(t, line(t, jobs["so-same"], folksentiaSample), line(t, jobs["so-other"], folksentiaSample))
	if _, err := p.ImportResults(ctx, pool, first, ImportOptions{}); err != nil {
		t.Fatal(err)
	}

	// so-same unchanged, so-new new, so-other with another BPM.
	path := resultsFile(t,
		line(t, jobs["so-same"], folksentiaSample),
		line(t, jobs["so-new"], folksentiaSample),
		line(t, jobs["so-other"], strings.Replace(folksentiaSample, `"bpm":124.0`, `"bpm":98.0`, 1)),
	)
	dry, err := p.ImportResults(ctx, pool, path, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Stored != 3 || dry.Changed != 2 || dry.ChangedBy["bpm"] != 2 || dry.ChangedBy["beats"] != 1 {
		t.Errorf("dry run: %+v", dry)
	}
	if bpm, _ := songBPM(t, pool, "so-new"); bpm.Valid {
		t.Error("a dry run stored a value")
	}
	if bpm, _ := songBPM(t, pool, "so-other"); bpm.Float64 != 124 {
		t.Errorf("a dry run changed a value: %v", bpm.Float64)
	}
	if errs := analysisErrors(t, pool, "so-new"); len(errs) != 0 {
		t.Errorf("a dry run stored analysis rows: %q", errs)
	}

	real, err := p.ImportResults(ctx, pool, path, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if real.Stored != dry.Stored || real.Changed != dry.Changed || real.Failed != dry.Failed || !maps.Equal(real.ChangedBy, dry.ChangedBy) {
		t.Errorf("import %+v, dry run said %+v", real, dry)
	}
	if bpm, _ := songBPM(t, pool, "so-other"); bpm.Float64 != 98 {
		t.Errorf("import: %v", bpm.Float64)
	}

	// Nothing left to change: a dry run and an import both say so.
	for _, dryRun := range []bool{true, false} {
		if rep, err := p.ImportResults(ctx, pool, path, ImportOptions{DryRun: dryRun}); err != nil || rep.Stored != 3 || rep.Changed != 0 || len(rep.ChangedBy) != 0 {
			t.Errorf("repeat (dry run %v): %+v %v", dryRun, rep, err)
		}
	}
}

// lockAway moves the songs' files into a directory nobody may enter, as
// /srv/music looks to a process without the music group.
func lockAway(t *testing.T, pool *sql.DB, dir string, ids ...string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		var p string
		if err := pool.QueryRow("SELECT path FROM songs WHERE id = ?", id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		np := filepath.Join(locked, filepath.Base(p))
		if err := os.Rename(p, np); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec("UPDATE songs SET path = ? WHERE id = ?", np, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
}

// A file the import cannot stat is "unreadable", not "stale" (#120).
func TestImportReportsUnreadableFiles(t *testing.T) {
	ctx := context.Background()
	pool, dir := library(t, "locked", "good", "gone", "changed")
	f := exportAll(t, pool, dir)
	jobs := map[string]OfflineJob{}
	for _, j := range f.Jobs {
		jobs[j.ID] = j
	}
	if err := os.Remove(filepath.Join(dir, "gone.flac")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "changed.flac"), []byte("longer audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockAway(t, pool, dir, "so-locked")

	path := resultsFile(t,
		line(t, jobs["so-locked"], folksentiaSample),
		line(t, jobs["so-good"], folksentiaSample),
		line(t, jobs["so-gone"], folksentiaSample),
		line(t, jobs["so-changed"], folksentiaSample),
	)
	rep, err := offlinePlan().ImportResults(ctx, pool, path, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stored != 1 || rep.Skipped["unreadable"] != 1 || rep.Skipped["stale"] != 2 {
		t.Errorf("report: %+v", rep)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "line 1:") || !strings.Contains(rep.Problems[0], "permission denied") {
		t.Errorf("problems: %q", rep.Problems)
	}
}

// When most files cannot be read, the import stops instead of skipping the
// whole library (#120). As on prod, a few readable files at the top of the
// music folder come first.
func TestImportStopsWhenTheMusicFolderIsUnreadable(t *testing.T) {
	ctx := context.Background()
	const top = 5
	names := make([]string, top+2*unreadableLimit)
	for i := range names {
		names[i] = fmt.Sprintf("t%02d", i)
	}
	pool, dir := library(t, names...)
	f := exportAll(t, pool, dir)
	byID := map[string]OfflineJob{}
	for _, j := range f.Jobs {
		byID[j.ID] = j
	}
	lines := make([]string, len(names))
	for i, n := range names {
		lines[i] = line(t, byID["so-"+n], folksentiaSample)
	}
	var locked []string
	for _, n := range names[top:] {
		locked = append(locked, "so-"+n)
	}
	lockAway(t, pool, dir, locked...)

	// Fewer unreadable files than the limit are only reported.
	if rep, err := offlinePlan().ImportResults(ctx, pool, resultsFile(t, lines[:top+unreadableLimit-1]...), ImportOptions{DryRun: true}); err != nil || rep.Skipped["unreadable"] != unreadableLimit-1 {
		t.Errorf("a few unreadable files: %v (%+v)", err, rep)
	}

	rep, err := offlinePlan().ImportResults(ctx, pool, resultsFile(t, lines...), ImportOptions{DryRun: true})
	if !errors.Is(err, ErrMusicUnreadable) {
		t.Fatalf("err %v, report %+v", err, rep)
	}
	if rep.Stored != top || rep.Skipped["unreadable"] != unreadableLimit {
		t.Errorf("dry run: %+v", rep)
	}
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM analysis").Scan(&n); err != nil || n != 0 {
		t.Errorf("a dry run stored analysis rows: %d %v", n, err)
	}
}
