package server

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	u, ok := s.requireSession(w, r)
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
	// startedAt, current and lastErr are for progress: when the job began,
	// the track being analysed, and the last failure ("song tool: message").
	startedAt time.Time
	current   string
	lastErr   string
	cancel    context.CancelFunc
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
	out := J{
		"kind": j.kind, "mode": j.mode, "running": j.active,
		"total": j.total, "done": j.done, "failed": j.failed, "updated": j.updated,
		"startedAt": j.started, "finishedAt": nullIfEmpty(j.finished), "error": nullIfEmpty(j.errMsg),
		"lastError": nullIfEmpty(j.lastErr),
	}
	if j.active {
		out["pending"] = max(0, j.total-j.done)
		out["current"] = nullIfEmpty(j.current)
		out["songsPerMinute"] = songsPerMinute(j.done, time.Since(j.startedAt))
	}
	return out
}

// songsPerMinute is the rate of a job, one decimal place; 0 before the first
// minute has passed, so the first estimate is not a spike.
func songsPerMinute(done int, elapsed time.Duration) float64 {
	if done == 0 || elapsed < time.Second {
		return 0
	}
	return math.Round(float64(done)/elapsed.Minutes()*10) / 10
}

// progressLine is the log line of a running job. It is written every 100
// tracks or every 10 minutes, whichever comes first.
func progressLine(kind string, done, failed, pending int, rate float64, current string) string {
	return fmt.Sprintf("%s: %d done, %d failed, %d pending, %.1f songs/min, current=%s",
		kind, done, failed, pending, rate, current)
}

// summaryLine is the last log line of a job.
func summaryLine(kind string, done, failed, updated int, elapsed time.Duration, reason string) string {
	line := fmt.Sprintf("%s: finished, %d done, %d failed, %d updated in %s",
		kind, done, failed, updated, elapsed.Round(time.Second))
	if reason != "" {
		line += " (" + reason + ")"
	}
	return line
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
	// A feature whose tool is offline only is never run here (#105); asking
	// for it explicitly is refused instead of starting an empty job.
	avail := p.Available()
	for _, f := range sel.Features {
		if t := p.Tool(f); avail[t] == analysis.OfflineOnly {
			return fmt.Errorf("%w: %s: %s is %s", errNoTool, f, t, analysis.OfflineOnly)
		}
	}
	todo, err := p.Select(context.Background(), s.Pool, sel)
	if err != nil {
		return err
	}
	if !sel.Pending {
		// A recalculation asked for by hand starts these tracks afresh.
		if err := p.ResetAttempts(context.Background(), s.Pool, todo); err != nil {
			return err
		}
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
	j.startedAt, j.current, j.lastErr = time.Now(), "", ""
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

// runJob analyses one track at a time; tools run at the lowest priority. It
// logs progress every 100 tracks or every 10 minutes, and a summary at the end.
func (s *State) runJob(ctx context.Context, p *analysis.Plan, cmds map[string]string, todo []analysis.Task) {
	j := &s.jobs
	started, lastLog := time.Now(), time.Now()
	defer func() {
		j.mu.Lock()
		j.active, j.finished, j.cancel, j.current = false, nowISO8601(), nil, ""
		if ctx.Err() != nil && j.errMsg == "" {
			j.errMsg = "stopped"
		}
		updated, kind, again := j.updated, j.kind, j.again && ctx.Err() == nil
		done, failed, reason := j.done, j.failed, j.errMsg
		j.again = false
		j.mu.Unlock()
		if updated > 0 {
			markLibraryModified(s.Pool)
			s.refreshDuplicates()
		}
		logging.Infof("%s", summaryLine(kind, done, failed, updated, time.Since(started), reason))
		if again {
			go s.queueAnalysis()
		}
	}()
	for _, t := range todo {
		if ctx.Err() != nil {
			return
		}
		j.mu.Lock()
		j.current = t.SongID
		j.mu.Unlock()
		out := p.RunTask(ctx, s.Pool, cmds, t)
		j.mu.Lock()
		j.done++
		if out.Failed {
			j.failed++
		}
		if out.Err != "" {
			j.lastErr = t.SongID + " " + out.Err
		}
		if out.Changed {
			j.updated++
		}
		done, failed, kind := j.done, j.failed, j.kind
		j.mu.Unlock()
		if done%100 == 0 || time.Since(lastLog) >= 10*time.Minute {
			lastLog = time.Now()
			logging.Infof("%s", progressLine(kind, done, failed, len(todo)-done, songsPerMinute(done, time.Since(started)), t.SongID))
		}
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
	var failed int
	if err := s.Pool.QueryRowContext(r.Context(),
		"SELECT COUNT(DISTINCT song_id) FROM analysis WHERE error IS NOT NULL").Scan(&failed); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	scan := J{"running": false}
	if s.Progress != nil {
		scan = J{"running": s.Progress.Running(), "count": s.Progress.Count()}
	}
	writeJSON(w, http.StatusOK, J{
		"artists": artists, "albums": albums, "songs": songs, "duration": durMs / 1000,
		"bpm":         J{"tag": bpmTag, "calc": bpmCalc, "missing": bpmMissing},
		"lastScan":    nsToAny(lastScan),
		"scan":        scan,
		"job":         s.jobs.status(),
		"failedSongs": failed,
		"ffmpeg":      ff,
		"analysis":    J{"auto": p.Auto, "features": p.Assignments(), "tools": tools},
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

// APILibraryFailed lists the tracks whose last analysis failed, newest first,
// with the tool and its error. `total` counts the tracks; the list has at most
// `limit` entries (default 200). Retry them with POST /api/library/analysis.
// Server paths in error messages are replaced by "<music>".
func (s *State) APILibraryFailed(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLibrarian(w, r); !ok {
		return
	}
	limit := 200
	if v := queryFirst(r, "limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeAPIError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	ctx := r.Context()
	var total int
	if err := s.Pool.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT song_id) FROM analysis WHERE error IS NOT NULL").Scan(&total); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	rows, err := s.Pool.QueryContext(ctx, `SELECT a.song_id, s.title, s.artist, a.tool, MAX(a.error), MAX(a.analysed_ms)
		FROM analysis a JOIN songs s ON s.id = a.song_id
		WHERE a.error IS NOT NULL
		GROUP BY a.song_id, a.tool
		ORDER BY MAX(a.analysed_ms) DESC, a.song_id, a.tool
		LIMIT ?`, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	songs := []J{}
	for rows.Next() {
		var id, title, artist, tool, msg string
		var ms int64
		if err := rows.Scan(&id, &title, &artist, &tool, &msg, &ms); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		for _, dir := range s.MusicDirs {
			// With the separator, so that /music does not match /musical.
			if d := strings.TrimSuffix(dir, "/"); d != "" {
				msg = strings.ReplaceAll(msg, d+"/", "<music>/")
			}
		}
		songs = append(songs, J{
			"id": id, "title": title, "artist": artist, "tool": tool, "error": msg,
			"analysedAt": time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	if err := rows.Err(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"total": total, "songs": songs})
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
