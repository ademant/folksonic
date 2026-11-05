package server

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/bpm"
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
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

// jobRunner runs one librarian job at a time (BPM calculation). Library
// scans are tracked by scanner.Progress; a job and a scan never overlap.
type jobRunner struct {
	mu       sync.Mutex
	kind     string // "bpm" while running or after the last run
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

// bpmCandidate is a song the BPM job will analyse.
type bpmCandidate struct {
	id, path   string
	durationMs int64
}

// Errors for job starts that are refused.
var (
	errBusy     = errors.New("a scan or another job is running")
	errBadMode  = errors.New(`mode must be "missing" or "all"`)
	errNoFFmpeg = errors.New("ffmpeg not available")
)

// startBPM starts a BPM job: mode "missing" analyses songs without any BPM,
// "all" also recalculates calculated values. Tag values are never touched.
func (s *State) startBPM(mode string) error {
	var where string
	switch mode {
	case "missing":
		where = "bpm IS NULL"
	case "all":
		where = "bpm IS NULL OR bpm_source = 'calc'"
	default:
		return errBadMode
	}
	ffmpeg, err := bpm.FindFFmpeg(s.FFmpegPath)
	if err != nil {
		return errors.Join(errNoFFmpeg, err)
	}

	j := &s.jobs
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.active || (s.Progress != nil && s.Progress.Running()) {
		return errBusy
	}
	rows, err := s.Pool.Query("SELECT id, path, duration_ms FROM songs WHERE " + where + " ORDER BY id")
	if err != nil {
		return err
	}
	var todo []bpmCandidate
	for rows.Next() {
		var c bpmCandidate
		if err := rows.Scan(&c.id, &c.path, &c.durationMs); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, c)
	}
	rows.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Field by field: the mutex is held and must not be overwritten.
	j.kind, j.mode, j.active, j.cancel = "bpm", mode, true, cancel
	j.total, j.done, j.failed, j.updated = len(todo), 0, 0, 0
	j.started, j.finished, j.errMsg = nowISO8601(), "", ""
	go s.runBPM(ctx, ffmpeg, todo)
	return nil
}

// runBPM analyses one track at a time; ffmpeg runs at the lowest priority.
func (s *State) runBPM(ctx context.Context, ffmpeg string, todo []bpmCandidate) {
	j := &s.jobs
	defer func() {
		j.mu.Lock()
		j.active, j.finished, j.cancel = false, nowISO8601(), nil
		if ctx.Err() != nil && j.errMsg == "" {
			j.errMsg = "stopped"
		}
		updated := j.updated
		j.mu.Unlock()
		if updated > 0 {
			markLibraryModified(s.Pool)
		}
		logging.Infof("bpm: finished, %d updated", updated)
	}()
	for _, c := range todo {
		if ctx.Err() != nil {
			return
		}
		ok := s.analyseOne(ctx, ffmpeg, c)
		j.mu.Lock()
		j.done++
		switch ok {
		case bpmFailed:
			j.failed++
		case bpmStored:
			j.updated++
		}
		j.mu.Unlock()
	}
}

type bpmResult int

const (
	bpmFailed bpmResult = iota
	bpmStored
	bpmSkipped // the song changed meanwhile (tag added, removed)
)

func (s *State) analyseOne(ctx context.Context, ffmpeg string, c bpmCandidate) bpmResult {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	samples, err := bpm.Decode(tctx, ffmpeg, c.path, float64(c.durationMs)/1000)
	if err != nil {
		if ctx.Err() == nil {
			logging.Warnf("bpm %s: %v", c.id, err)
		}
		return bpmFailed
	}
	tempo, conf, err := bpm.Estimate(samples)
	if err != nil {
		logging.Warnf("bpm %s: %v", c.id, err)
		return bpmFailed
	}
	// The condition keeps tag values safe if a scan added one meanwhile.
	res, err := s.Pool.ExecContext(ctx,
		`UPDATE songs SET bpm = ?, bpm_source = 'calc', bpm_confidence = ?, bpm_at = ?
		 WHERE id = ? AND (bpm IS NULL OR bpm_source = 'calc')`,
		tempo, conf, nowISO8601(), c.id)
	if err != nil {
		return bpmFailed
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return bpmSkipped
	}
	return bpmStored
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
		COUNT(*) FILTER (WHERE bpm_source = 'calc'),
		COUNT(*) FILTER (WHERE bpm IS NULL),
		(SELECT value FROM settings WHERE key = ?)
		FROM songs`, scanner.LastScanKey).Scan(&artists, &albums, &songs, &durMs, &bpmTag, &bpmCalc, &bpmMissing, &lastScan)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	ff := J{"available": true}
	if _, err := bpm.FindFFmpeg(s.FFmpegPath); err != nil {
		ff = J{"available": false, "reason": err.Error()}
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
	case errors.Is(err, errNoFFmpeg):
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
