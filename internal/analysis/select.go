package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// Run is one tool call for one track: the group and the features to ask for.
type Run struct {
	Group    *Group
	Features []string
}

// Task is the work for one track.
type Task struct {
	SongID     string
	Path       string
	DurationMs int64
	Runs       []Run
}

// Selection picks tracks and features for a job.
type Selection struct {
	// IDs limits the job to these songs; nil means every song.
	IDs []string
	// Features limits the job to these features; nil means all assigned.
	Features []string
	// Pending selects only features that have no result from their tool yet
	// (failed runs count as results, so they are not retried every scan).
	// Otherwise the tools run again regardless of earlier results.
	Pending bool
	// MissingBPM selects songs without any BPM for the bpm feature, the
	// library page's "calculate missing" button. Earlier failures are retried.
	MissingBPM bool
	// Tool selects the tasks of one tool served by analysis workers. Empty
	// selects the tools this server runs itself, and never the worker tools.
	Tool string
}

// Select returns the tasks for sel, ordered by song id.
func (p *Plan) Select(ctx context.Context, db *sql.DB, sel Selection) ([]Task, error) {
	byID := map[string]*Task{}
	var order []string
	for _, g := range p.Groups {
		if g.worker != (sel.Tool != "") || (sel.Tool != "" && g.Tool != sel.Tool) {
			continue
		}
		features := g.Features
		if sel.Features != nil {
			features = slices.DeleteFunc(slices.Clone(features), func(f string) bool { return !slices.Contains(sel.Features, f) })
		}
		if len(features) == 0 {
			continue
		}
		where, args := p.condition(g, features, sel)
		q := "SELECT id, path, duration_ms FROM songs s WHERE (" + where + ")"
		if sel.IDs != nil {
			ids, _ := json.Marshal(sel.IDs)
			q += " AND s.id IN (SELECT value FROM json_each(?))"
			args = append(args, string(ids))
		}
		rows, err := db.QueryContext(ctx, q+" ORDER BY id", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t Task
			if err := rows.Scan(&t.SongID, &t.Path, &t.DurationMs); err != nil {
				rows.Close()
				return nil, err
			}
			if byID[t.SongID] == nil {
				byID[t.SongID] = &t
				order = append(order, t.SongID)
			}
			byID[t.SongID].Runs = append(byID[t.SongID].Runs, Run{Group: g, Features: features})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	slices.Sort(order)
	out := make([]Task, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out, nil
}

// condition is the SQL selecting the songs group g must run for.
func (p *Plan) condition(g *Group, features []string, sel Selection) (string, []any) {
	builtin := g.Tool == config.ToolBuiltin
	switch {
	case sel.MissingBPM && slices.Contains(features, "bpm"):
		return "s.bpm IS NULL", nil
	case builtin && sel.Pending:
		// The estimator never replaces a BPM that is already there.
		return "s.bpm IS NULL AND NOT EXISTS (SELECT 1 FROM analysis a WHERE a.song_id = s.id AND a.feature = 'bpm' AND a.tool = 'builtin')", nil
	case builtin:
		return "s.bpm IS NULL OR s.bpm_source = 'calc'", nil
	case sel.Pending:
		args := []any{g.Tool}
		for _, f := range features {
			args = append(args, f)
		}
		args = append(args, len(features))
		return "(SELECT COUNT(*) FROM analysis a WHERE a.song_id = s.id AND a.tool = ? AND a.feature IN (" +
			strings.TrimSuffix(strings.Repeat("?,", len(features)), ",") + ")) < ?", args
	default:
		return "1", nil
	}
}

// PendingFeatures returns the assigned features of one song that have no
// result from their tool yet.
func (p *Plan) PendingFeatures(ctx context.Context, db *sql.DB, songID string) ([]string, error) {
	tasks, err := p.Select(ctx, db, Selection{IDs: []string{songID}, Pending: true})
	if err != nil || len(tasks) == 0 {
		return nil, err
	}
	var out []string
	for _, r := range tasks[0].Runs {
		for _, f := range r.Features {
			var n int
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM analysis WHERE song_id = ? AND feature = ? AND tool = ?",
				songID, f, r.Group.Tool).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				out = append(out, f)
			}
		}
	}
	return out, nil
}
