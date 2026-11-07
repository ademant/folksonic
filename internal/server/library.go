package server

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

//go:embed static/library.html
var libraryPage []byte

// ServeLibraryPage serves the librarians' library page. Like the account page
// it is static and calls the JSON API with the session token.
func (s *State) ServeLibraryPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
	_, _ = w.Write(libraryPage)
}

// requireLibrarian is requireUser for the library administration API.
func (s *State) requireLibrarian(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return u, false
	}
	if u.Role != users.RoleLibrarian {
		writeAPIError(w, http.StatusForbidden, "librarians only")
		return u, false
	}
	return u, true
}

// ── Background jobs ─────────────────────────────────────────────────────────

// jobRunner runs one background job at a time: analysis started by a
// librarian (the BPM buttons, recalculation) or queued after a scan. Library
// scans are tracked by scanner.Progress; a job and a scan never overlap.
type jobRunner struct {
	mu       sync.Mutex
	kind     string // "bpm" or "analysis", while running or after the last run
	mode     string
	active   bool
	total    int
	done     int
	failed   int
	updated  int
	started  string
	finished string
	errMsg   string
	cancel   context.CancelFunc
	// again asks for one more pending-analysis pass once the running job or
	// scan has finished.
	again bool
	// lastUnavailable is the last "tool not available" message logged by
	// the automatic analysis, so it is not repeated after every scan.
	lastUnavailable string
}

func (j *jobRunner) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.active
}

func (j *jobRunner) status() J {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.kind == "" {
		return J{"running": false}
	}
	return J{
		"kind": j.kind, "mode": j.mode, "running": j.active,
		"total": j.total, "done": j.done, "failed": j.failed, "updated": j.updated,
		"startedAt": j.started, "finishedAt": nullIfEmpty(j.finished), "error": nullIfEmpty(j.errMsg),
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Errors for job starts that are refused.
var (
	errBusy    = errors.New("a scan or another job is running")
	errBadMode = errors.New(`mode must be "missing" or "all"`)
	errNoTool  = analysis.ErrUnavailable
)

// plan is the analysis plan from the config. Without one (tests, or a State
// built by hand) it is the default: built-in BPM on ffmpeg_path, by hand.
func (s *State) plan() *analysis.Plan {
	if s.Analysis != nil {
		return s.Analysis
	}
	return analysis.NewPlan(config.Analysis{BPM: config.ToolBuiltin}, s.FFmpegPath)
}

// startBPM starts a BPM job: mode "missing" analyses songs without any BPM,
// "all" recalculates with the tool assigned to bpm. The built-in estimator
// never replaces a tag value; an assigned external tool does.
func (s *State) startBPM(mode string) error {
	if mode != "missing" && mode != "all" {
		return errBadMode
	}
	return s.startJob("bpm", mode, analysis.Selection{Features: []string{"bpm"}, MissingBPM: mode == "missing"})
}

// startJob selects the tasks and starts them in the background.
func (s *State) startJob(kind, mode string, sel analysis.Selection) error {
	p := s.plan()
	j := &s.jobs
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active || (s.Progress != nil && s.Progress.Running()) {
		return errBusy
	}
	if sel.Features != nil && slices.ContainsFunc(sel.Features, func(f string) bool { return p.Tool(f) == "" }) {
		return fmt.Errorf("%w: %v", errNotAssigned, sel.Features)
	}
	todo, err := p.Select(context.Background(), s.Pool, sel)
	if err != nil {
		return err
	}
	cmds, err := p.Commands(todo)
	if err != nil {
		return err
	}
	s.launch(kind, mode, p, cmds, todo)
	return nil
}

// errNotAssigned is returned for a feature no tool is assigned to.
var errNotAssigned = errors.New("no analysis tool is assigned to a requested feature")

// launch starts the job; j.mu must be held.
func (s *State) launch(kind, mode string, p *analysis.Plan, cmds map[string]string, todo []analysis.Task) {
	j := &s.jobs
	ctx, cancel := context.WithCancel(context.Background())
	// Field by field: the mutex is held and must not be overwritten.
	j.kind, j.mode, j.active, j.cancel = kind, mode, true, cancel
	j.total, j.done, j.failed, j.updated = len(todo), 0, 0, 0
	j.started, j.finished, j.errMsg = nowISO8601(), "", ""
	go s.runJob(ctx, p, cmds, todo)
}

// queueAnalysis starts analysing pending features when automatic analysis
// is on. It is called after scans and watcher updates; while a scan or a job
// runs, the pass is deferred until it ends.
func (s *State) queueAnalysis() {
	p := s.plan()
	if !p.Auto {
		return
	}
	j := &s.jobs
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active || (s.Progress != nil && s.Progress.Running()) {
		j.again = true
		return
	}
	todo, err := p.Select(context.Background(), s.Pool, analysis.Selection{Pending: true})
	if err != nil {
		logging.Errorf("analysis: selecting pending tracks: %v", err)
		return
	}
	if len(todo) == 0 {
		return
	}
	cmds, err := p.Commands(todo)
	if err != nil {
		if err.Error() != j.lastUnavailable {
			logging.Warnf("analysis: %d tracks pending, not started: %v", len(todo), err)
			j.lastUnavailable = err.Error()
		}
		return
	}
	j.lastUnavailable = ""
	logging.Infof("analysis: %d tracks pending", len(todo))
	s.launch("analysis", "pending", p, cmds, todo)
}

// runJob analyses one track at a time; tools run at the lowest priority.
func (s *State) runJob(ctx context.Context, p *analysis.Plan, cmds map[string]string, todo []analysis.Task) {
	j := &s.jobs
	defer func() {
		j.mu.Lock()
		j.active, j.finished, j.cancel = false, nowISO8601(), nil
		if ctx.Err() != nil && j.errMsg == "" {
			j.errMsg = "stopped"
		}
		updated, kind, again := j.updated, j.kind, j.again && ctx.Err() == nil
		j.again = false
		j.mu.Unlock()
		if updated > 0 {
			markLibraryModified(s.Pool)
		}
		logging.Infof("%s: finished, %d updated", kind, updated)
		if again {
			go s.queueAnalysis()
		}
	}()
	for _, t := range todo {
		if ctx.Err() != nil {
			return
		}
		out := p.RunTask(ctx, s.Pool, cmds, t)
		j.mu.Lock()
		j.done++
		if out.Failed {
			j.failed++
		}
		if out.Changed {
			j.updated++
		}
		j.mu.Unlock()
	}
}

func markLibraryModified(db *sql.DB) {
	_, _ = db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		scanner.LibraryModifiedKey, nowISO8601())
}

// ── API ─────────────────────────────────────────────────────────────────────

// APILibraryStats returns library totals, BPM coverage, the last scan, the
// running scan or job, and whether BPM calculation is available.
func (s *State) APILibraryStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	var artists, albums, songs, durMs, bpmTag, bpmCalc, bpmMissing int64
	var lastScan sql.NullString
	err := s.Pool.QueryRowContext(r.Context(), `SELECT
		(SELECT COUNT(*) FROM artists), (SELECT COUNT(*) FROM albums), COUNT(*),
		COALESCE(SUM(duration_ms), 0),
		COUNT(*) FILTER (WHERE bpm_source = 'tag'),
		COUNT(*) FILTER (WHERE bpm_source IS NOT NULL AND bpm_source <> 'tag'),
		COUNT(*) FILTER (WHERE bpm IS NULL),
		(SELECT value FROM settings WHERE key = ?)
		FROM songs`, scanner.LastScanKey).Scan(&artists, &albums, &songs, &durMs, &bpmTag, &bpmCalc, &bpmMissing, &lastScan)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	// "ffmpeg" is whether the BPM buttons can work: the tool assigned to bpm
	// (the built-in estimator decodes with ffmpeg) is available.
	p := s.plan()
	avail := p.Available()
	ff := J{"available": true}
	switch tool := p.Tool("bpm"); {
	case tool == "":
		ff = J{"available": false, "reason": "no tool is assigned to bpm"}
	case avail[tool] != "":
		ff = J{"available": false, "reason": avail[tool]}
	}
	tools := J{}
	for tool, reason := range avail {
		tools[tool] = J{"available": reason == "", "reason": nullIfEmpty(reason)}
	}
	scan := J{"running": false}
	if s.Progress != nil {
		scan = J{"running": s.Progress.Running(), "count": s.Progress.Count()}
	}
	writeJSON(w, http.StatusOK, J{
		"artists": artists, "albums": albums, "songs": songs, "duration": durMs / 1000,
		"bpm":      J{"tag": bpmTag, "calc": bpmCalc, "missing": bpmMissing},
		"lastScan": nsToAny(lastScan),
		"scan":     scan,
		"job":      s.jobs.status(),
		"ffmpeg":   ff,
		"analysis": J{"auto": p.Auto, "features": p.Assignments(), "tools": tools},
	})
}

// APILibraryScan starts a rescan of the music folders.
func (s *State) APILibraryScan(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	s.jobs.mu.Lock()
	busy := s.jobs.active || s.Progress == nil || !s.Progress.Begin()
	s.jobs.mu.Unlock()
	if busy {
		writeAPIError(w, http.StatusConflict, errBusy.Error())
		return
	}
	go func() {
		if _, err := scanner.Scan(s.Pool, s.MusicDirs, s.CoversDir, s.SongTags, s.Progress); err != nil {
			logging.Errorf("scan: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, J{"status": "started"})
}

// APILibraryBPM starts BPM calculation: {"mode": "missing"|"all"}.
func (s *State) APILibraryBPM(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	switch err := s.startBPM(body.Mode); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, J{"status": "started", "job": s.jobs.status()})
	case errors.Is(err, errBadMode):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errBusy):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errNoTool), errors.Is(err, errNotAssigned):
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "database error")
	}
}

// APILibraryAnalysis re-runs the assigned tools regardless of earlier
// results: {"ids": ["so-…"]} or {"all": true}, optionally limited to
// {"features": ["key", …]}.
func (s *State) APILibraryAnalysis(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	var body struct {
		IDs      []string `json:"ids"`
		All      bool     `json:"all"`
		Features []string `json:"features"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.All == (len(body.IDs) > 0) {
		writeAPIError(w, http.StatusBadRequest, `give either "ids" or "all": true`)
		return
	}
	sel := analysis.Selection{IDs: body.IDs, Features: body.Features}
	if len(body.Features) == 0 {
		sel.Features = nil
	}
	if body.All {
		sel.IDs = nil
	}
	switch err := s.startJob("analysis", "recalc", sel); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, J{"status": "started", "job": s.jobs.status()})
	case errors.Is(err, errNotAssigned):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errBusy):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errNoTool):
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "database error")
	}
}

// APILibraryCancel stops the running job. Scans cannot be stopped.
func (s *State) APILibraryCancel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	s.jobs.mu.Lock()
	cancel := s.jobs.cancel
	s.jobs.mu.Unlock()
	if cancel == nil {
		writeAPIError(w, http.StatusConflict, "no job is running")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, J{"status": "stopping"})
}
