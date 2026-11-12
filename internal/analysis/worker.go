package analysis

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// Analysis workers run a tool on another machine and send the results back
// (#78). The server leases a track to a worker for a while, counts each lease
// as an attempt, and stores the result after checking it against the lease
// and the file's fingerprint.

// WorkerLease is how long a worker holds a track before it may be given to
// another worker. A worker that finishes earlier sends its result.
const WorkerLease = 30 * time.Minute

// Stamp is a file's size and modification time in milliseconds: the
// fingerprint the analysis belongs to.
type Stamp struct {
	Size    int64
	MtimeMs int64
}

// stampOf returns the fingerprint of a file, or nil when it is unknown.
func stampOf(info os.FileInfo) *Stamp {
	if info == nil {
		return nil
	}
	return &Stamp{Size: info.Size(), MtimeMs: info.ModTime().UnixMilli()}
}

var (
	// ErrNotWorkerTool is returned for a tool that is not served by workers.
	ErrNotWorkerTool = errors.New("tool is not served by analysis workers")
	// ErrLeaseNotHeld is returned for a result from a key that holds no lease
	// on the track: the lease expired and was given to someone else, or the
	// track was never leased to this key.
	ErrLeaseNotHeld = errors.New("the track is not leased to this worker")
	// ErrFileChanged is returned for a result about audio that has since
	// changed. The lease is released; the next scan picks the track up.
	ErrFileChanged = errors.New("the file changed after the job was given out")
)

// WorkerJob is one track a worker is asked to analyse.
type WorkerJob struct {
	SongID     string
	Path       string
	Stamp      Stamp
	DurationMs int64
	Features   []string
}

// WorkerResult is a worker's answer for one track. Output is the tool's
// stdout; Error is set instead when the worker could not run the tool.
type WorkerResult struct {
	SongID string
	Tool   string
	KeyID  int64
	Stamp  Stamp
	Output []byte
	Error  string
}

// LeaseWorkerJobs gives up to limit pending tracks of tool to the worker key
// keyID. Tracks with a live lease, and tracks without a known fingerprint, are
// skipped. Each lease counts as an attempt; a track that has used up its
// attempts is recorded as failed instead of being leased again.
func (p *Plan) LeaseWorkerJobs(ctx context.Context, db *sql.DB, tool string, keyID int64, limit int, now time.Time) ([]WorkerJob, error) {
	g := p.group(tool)
	if g == nil || !g.worker {
		return nil, ErrNotWorkerTool
	}
	tasks, err := p.Select(ctx, db, Selection{Tool: tool, Pending: true})
	if err != nil {
		return nil, err
	}
	nowMs := now.UnixMilli()
	var jobs []WorkerJob
	for _, t := range tasks {
		if len(jobs) >= limit {
			break
		}
		var size, mtime sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT file_size, file_mtime_ms FROM songs WHERE id = ?", t.SongID).Scan(&size, &mtime); err != nil {
			return nil, err
		}
		if !size.Valid || !mtime.Valid {
			continue // a scan records the fingerprint first
		}
		var live int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM analysis_leases WHERE song_id = ? AND tool = ? AND until_ms > ?",
			t.SongID, tool, nowMs).Scan(&live); err != nil {
			return nil, err
		}
		if live > 0 {
			continue
		}
		run := t.Runs[0]
		attempts, err := attemptCount(ctx, db, t.SongID, tool)
		if err != nil {
			return nil, err
		}
		if attempts >= maxAttempts {
			gaveUp := fmt.Sprintf("gave up after %d interrupted attempts", attempts)
			if _, err := store(ctx, db, t.SongID, Run{Group: g, Features: run.Features}, "", nil, errors.New(gaveUp), nil); err != nil {
				return nil, err
			}
			continue
		}
		// Taken only if no live lease appeared meanwhile; an expired one is
		// replaced.
		res, err := db.ExecContext(ctx, `INSERT INTO analysis_leases (song_id, tool, key_id, until_ms) VALUES (?, ?, ?, ?)
			ON CONFLICT(song_id, tool) DO UPDATE SET key_id = excluded.key_id, until_ms = excluded.until_ms
			WHERE analysis_leases.until_ms <= ?`,
			t.SongID, tool, keyID, now.Add(WorkerLease).UnixMilli(), nowMs)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := setAttemptCount(ctx, db, t.SongID, tool, attempts+1); err != nil {
			return nil, err
		}
		jobs = append(jobs, WorkerJob{
			SongID: t.SongID, Path: t.Path, DurationMs: t.DurationMs,
			Stamp:    Stamp{Size: size.Int64, MtimeMs: mtime.Int64},
			Features: run.Features,
		})
	}
	return jobs, nil
}

// StoreWorkerResult records a worker's answer. The key must hold the lease,
// and the file must still have the fingerprint the job was given out with.
// The output is parsed with the same code as a local run, and BPM values
// outside 20–300 are refused. It reports whether a stored value changed.
func (p *Plan) StoreWorkerResult(ctx context.Context, db *sql.DB, res WorkerResult) (changed bool, err error) {
	g := p.group(res.Tool)
	if g == nil || !g.worker {
		return false, ErrNotWorkerTool
	}
	var holder int64
	err = db.QueryRowContext(ctx,
		"SELECT key_id FROM analysis_leases WHERE song_id = ? AND tool = ?", res.SongID, res.Tool).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && holder != res.KeyID) {
		return false, ErrLeaseNotHeld
	}
	if err != nil {
		return false, err
	}
	var size, mtime sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT file_size, file_mtime_ms FROM songs WHERE id = ?", res.SongID).Scan(&size, &mtime); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, errSongGone
		}
		return false, err
	}
	if !size.Valid || !mtime.Valid || size.Int64 != res.Stamp.Size || mtime.Int64 != res.Stamp.MtimeMs {
		if _, err := db.ExecContext(ctx, "DELETE FROM analysis_leases WHERE song_id = ? AND tool = ?", res.SongID, res.Tool); err != nil {
			return false, err
		}
		return false, ErrFileChanged
	}

	features := g.Features
	run := Run{Group: g, Features: features}
	var vals Values
	var runErr error
	if res.Error != "" {
		runErr = errors.New(shortMessage(res.Error))
	} else {
		vals, runErr = parseFolksentia(res.Output, features)
		if runErr == nil {
			runErr = checkRanges(vals)
		}
	}
	version := g.adapter.version(ctx, "")
	return store(ctx, db, res.SongID, run, version, vals, runErr, &res.Stamp)
}

// checkRanges refuses values no track can have.
func checkRanges(vals Values) error {
	if b, ok := vals["bpm"].(*BPM); ok && (b.Value < 20 || b.Value > 300) {
		return fmt.Errorf("bpm %.1f outside 20–300", b.Value)
	}
	return nil
}
