package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// Outcome is the result of one task.
type Outcome struct {
	Failed  bool // at least one tool failed
	Changed bool // a stored value changed
}

// Commands resolves the command of every tool the tasks need. It fails when
// one is not available, before any work starts.
func (p *Plan) Commands(tasks []Task) (map[string]string, error) {
	cmds := map[string]string{}
	for _, t := range tasks {
		for _, r := range t.Runs {
			if _, ok := cmds[r.Group.Tool]; ok {
				continue
			}
			c, err := p.resolve(r.Group)
			if err != nil {
				return nil, err
			}
			cmds[r.Group.Tool] = c
		}
	}
	return cmds, nil
}

// maxAttempts is how many runs of one tool on one track may end without a
// stored result (the process was killed, or folksonic stopped) before the
// track is given up on. Without it, a track that always kills its tool is
// retried after every restart, forever (#75).
const maxAttempts = 2

// RunTask runs the tools of one track and stores their results. It writes
// one log line per tool run.
func (p *Plan) RunTask(ctx context.Context, db *sql.DB, cmds map[string]string, t Task) Outcome {
	var out Outcome
	// The file's size and mtime are taken before the tools read it, so a
	// change while they run makes the next scan analyse it again.
	info, statErr := os.Stat(t.Path)
	for _, r := range t.Runs {
		if ctx.Err() != nil {
			return out
		}
		g := r.Group
		if g.maxDuration > 0 && time.Duration(t.DurationMs)*time.Millisecond > g.maxDuration {
			// Recorded as a result, so the track is not selected again; the
			// file's size and mtime are not stored, since nothing was analysed.
			reason := fmt.Sprintf("longer than max_duration %s", g.maxDuration)
			changed, serr := store(ctx, db, t.SongID, r, "", nil, errors.New(reason), nil)
			if serr != nil {
				logging.Warnf("analysis song=%s tool=%s store_error=%q", t.SongID, g.Tool, serr.Error())
				out.Failed = true
			} else {
				logging.Infof("analysis song=%s tool=%s skipped: %s", t.SongID, g.Tool, reason)
			}
			out.Changed = out.Changed || changed
			continue
		}
		attempts, err := attemptCount(ctx, db, t.SongID, g.Tool)
		if err != nil {
			logging.Warnf("analysis song=%s tool=%s attempts_error=%q", t.SongID, g.Tool, err.Error())
			out.Failed = true
			continue
		}
		if attempts >= maxAttempts {
			gaveUp := fmt.Sprintf("gave up after %d interrupted attempts", attempts)
			changed, serr := store(ctx, db, t.SongID, r, "", nil, errors.New(gaveUp), nil)
			if serr != nil {
				logging.Warnf("analysis song=%s tool=%s store_error=%q", t.SongID, g.Tool, serr.Error())
			} else {
				logging.Warnf("analysis song=%s tool=%s %s", t.SongID, g.Tool, gaveUp)
			}
			out.Failed = true
			out.Changed = out.Changed || changed
			continue
		}
		// The attempt is recorded before the tool starts. If folksonic dies
		// during the run, the count stays and the next run sees it.
		if err := setAttemptCount(ctx, db, t.SongID, g.Tool, attempts+1); err != nil {
			logging.Warnf("analysis song=%s tool=%s attempts_error=%q", t.SongID, g.Tool, err.Error())
			out.Failed = true
			continue
		}
		start := time.Now()
		version := p.version(ctx, g, cmds[g.Tool])
		var vals Values
		err = statErr
		if err == nil {
			tctx, cancel := context.WithTimeout(ctx, g.timeout)
			vals, err = g.adapter.run(tctx, cmds[g.Tool], t.Path, float64(t.DurationMs)/1000, r.Features, g.memLimit)
			if err != nil && tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				err = fmt.Errorf("timed out after %s", g.timeout)
			}
			cancel()
		}
		if ctx.Err() != nil {
			// Stopped on purpose: store nothing, the task stays pending, and
			// the stopped run does not count as an interrupted attempt.
			_ = setAttemptCount(context.Background(), db, t.SongID, g.Tool, attempts)
			return out
		}
		changed, serr := store(ctx, db, t.SongID, r, version, vals, err, info)
		elapsed := time.Since(start).Milliseconds()
		switch {
		case serr != nil:
			logging.Warnf("analysis song=%s tool=%s store_error=%q", t.SongID, g.Tool, serr.Error())
			out.Failed = true
		case err != nil:
			logging.Warnf("analysis song=%s tool=%s version=%s ms=%d error=%q", t.SongID, g.Tool, version, elapsed, err.Error())
			out.Failed = true
		default:
			logging.Infof("analysis song=%s tool=%s version=%s ms=%d %s", t.SongID, g.Tool, version, elapsed, summary(r.Features, vals))
		}
		out.Changed = out.Changed || changed
	}
	return out
}

// attemptCount returns how many runs of tool on songID ended without a result.
func attemptCount(ctx context.Context, db *sql.DB, songID, tool string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT attempts FROM analysis_attempts WHERE song_id = ? AND tool = ?", songID, tool).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// setAttemptCount sets the count; 0 removes the row.
func setAttemptCount(ctx context.Context, db *sql.DB, songID, tool string, n int) error {
	if n <= 0 {
		_, err := db.ExecContext(ctx, "DELETE FROM analysis_attempts WHERE song_id = ? AND tool = ?", songID, tool)
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO analysis_attempts (song_id, tool, attempts, started_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(song_id, tool) DO UPDATE SET attempts = excluded.attempts, started_ms = excluded.started_ms`,
		songID, tool, n, time.Now().UnixMilli())
	return err
}

// ResetAttempts clears the interrupted-attempt counts of tasks. A recalculation
// asked for by hand starts those tracks afresh; automatic passes keep the
// counts.
func (p *Plan) ResetAttempts(ctx context.Context, db *sql.DB, tasks []Task) error {
	for _, t := range tasks {
		for _, r := range t.Runs {
			if err := setAttemptCount(ctx, db, t.SongID, r.Group.Tool, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// errSongGone is returned by store when the song was removed meanwhile.
var errSongGone = fmt.Errorf("song no longer in the library")

// store records one tool run: an analysis row per feature, and the values
// that are known. A failed run records the error and keeps the values.
func store(ctx context.Context, db *sql.DB, songID string, r Run, version string, vals Values, runErr error, info os.FileInfo) (changed bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM songs WHERE id = ?", songID).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, errSongGone
	}
	now := time.Now().UnixMilli()
	var errText sql.NullString
	if runErr != nil {
		errText = sql.NullString{String: shortMessage(runErr.Error()), Valid: true}
	}
	// The run ended with a result, so it no longer counts as interrupted.
	if _, err := tx.ExecContext(ctx, "DELETE FROM analysis_attempts WHERE song_id = ? AND tool = ?", songID, r.Group.Tool); err != nil {
		return false, err
	}
	for _, f := range r.Features {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO analysis (song_id, feature, tool, tool_version, analysed_ms, error) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(song_id, feature) DO UPDATE SET tool = excluded.tool, tool_version = excluded.tool_version,
			   analysed_ms = excluded.analysed_ms, error = excluded.error`,
			songID, f, r.Group.Tool, version, now, errText); err != nil {
			return false, err
		}
		v, ok := vals[f]
		if runErr != nil || !ok || v == nil {
			continue
		}
		var res sql.Result
		if f == "bpm" {
			b := v.(*BPM)
			source, cond := r.Group.Tool, ""
			if r.Group.Tool == config.ToolBuiltin {
				// The estimator never replaces a BPM tag or another tool's value.
				source, cond = "calc", " AND (bpm IS NULL OR bpm_source = 'calc')"
			}
			res, err = tx.ExecContext(ctx,
				`UPDATE songs SET bpm = ?, bpm_source = ?, bpm_confidence = ?, bpm_at = ?
				 WHERE id = ? AND (bpm IS NOT ? OR bpm_source IS NOT ? OR bpm_confidence IS NOT ?)`+cond,
				b.Value, source, b.Confidence, time.Now().UTC().Format("2006-01-02T15:04:05Z"),
				songID, b.Value, source, b.Confidence)
		} else {
			js, jerr := json.Marshal(v)
			if jerr != nil {
				return false, jerr
			}
			res, err = setValue(ctx, tx, songID, f, string(js), now)
		}
		if err != nil {
			return false, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			changed = true
		}
	}
	if info != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE songs SET file_size = ?, file_mtime_ms = ? WHERE id = ?",
			info.Size(), info.ModTime().UnixMilli(), songID); err != nil {
			return false, err
		}
	}
	return changed, tx.Commit()
}

// execer is satisfied by *sql.Tx and *sql.DB.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// setValue stores a value; changed_ms moves only when the value differs.
func setValue(ctx context.Context, tx execer, songID, name, value string, nowMs int64) (sql.Result, error) {
	return tx.ExecContext(ctx,
		`INSERT INTO song_values (song_id, name, value, changed_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(song_id, name) DO UPDATE SET value = excluded.value, changed_ms = excluded.changed_ms
		 WHERE song_values.value IS NOT excluded.value`,
		songID, name, value, nowMs)
}

// summary is the key=value part of a log line: scalars as they are, long
// lists as their length.
func summary(features []string, vals Values) string {
	var parts []string
	f2 := func(p *float64) string {
		if p == nil {
			return "null"
		}
		return strconv.FormatFloat(*p, 'f', 2, 64)
	}
	i2 := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return strconv.FormatInt(*p, 10)
	}
	for _, f := range features {
		v, ok := vals[f]
		if !ok || v == nil {
			parts = append(parts, f+"=null")
			continue
		}
		switch x := v.(type) {
		case *BPM:
			parts = append(parts, "bpm="+strconv.FormatFloat(x.Value, 'f', 1, 64), "bpm_confidence="+f2(x.Confidence))
		case *Key:
			parts = append(parts, fmt.Sprintf("key=%q", x.Tonic+" "+x.Mode), "key_confidence="+f2(x.Confidence))
		case *Loudness:
			parts = append(parts, "lufs="+f2(x.IntegratedLufs), "true_peak="+f2(x.TruePeakDbtp))
		case *Silence:
			parts = append(parts, "lead_ms="+i2(x.LeadMs), "tail_ms="+i2(x.TailMs))
		case *Beats:
			parts = append(parts, "beats="+strconv.Itoa(len(x.TimesMs)), "downbeats="+strconv.Itoa(len(x.DownbeatIndexes)))
		case *Waveform:
			parts = append(parts, "waveform_bins="+strconv.Itoa(x.Bins))
		case *Fingerprint:
			parts = append(parts, "fingerprint_sec="+strconv.FormatFloat(x.DurationSec, 'f', 0, 64))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}
