package analysis

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Offline analysis (#105): the server exports a job list, folksentia runs on
// another machine against its own copy of the library and writes one result
// line per job, and the server imports those lines. Nothing talks to the
// server over the network.

// Job file identification. An import refuses lines from another format.
const (
	JobsFormat  = "tinysonic-analysis-jobs"
	JobsVersion = 1
	// OfflineTool is the only tool that can run offline.
	OfflineTool = "folksentia"
	// folksentiaSchema is the output schema the results are read with.
	folksentiaSchema = 1
	// OfflineOnly is the availability of a folksentia without a command.
	OfflineOnly = "offline only: run it with `tinysonic analysis export` and `import`"
)

// Stamp is a file's size and modification time in milliseconds: the
// fingerprint the analysis belongs to.
type Stamp struct {
	Size    int64 `json:"size"`
	MtimeMs int64 `json:"mtimeMs"`
}

// stampOf returns the fingerprint of a file, or nil when it is unknown.
func stampOf(info os.FileInfo) *Stamp {
	if info == nil {
		return nil
	}
	return &Stamp{Size: info.Size(), MtimeMs: info.ModTime().UnixMilli()}
}

// JobsFile is the exported job list.
type JobsFile struct {
	Format  string            `json:"format"`
	Version int               `json:"version"`
	Created string            `json:"created"`
	Tool    string            `json:"tool"`
	Schema  int               `json:"schema"`
	Options FolksentiaOptions `json:"options"`
	Jobs    []OfflineJob      `json:"jobs"`
}

// FolksentiaOptions are the folksentia flags for the exported features, the
// same ones folksentiaArgs passes for a local run.
type FolksentiaOptions struct {
	Downbeats   bool `json:"downbeats,omitempty"`
	Waveform    int  `json:"waveform,omitempty"`
	Fingerprint bool `json:"fingerprint,omitempty"`
}

// OfflineJob is one track to analyse. Path is relative to the music folder
// and /-separated.
type OfflineJob struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	MtimeMs    int64    `json:"mtimeMs"`
	DurationMs int64    `json:"durationMs"`
	Features   []string `json:"features"`
}

// ErrNoOfflineTool is returned when folksentia is not assigned to a feature.
var ErrNoOfflineTool = errors.New("folksentia is not assigned to any analysis feature; nothing can be exported")

// ExportReport counts what an export left out.
type ExportReport struct {
	Jobs int
	// Unreadable counts tracks whose file could not be read on the server.
	Unreadable int
	// Outside counts tracks that lie in no music folder.
	Outside int
}

// ExportJobs selects folksentia's tracks for sel (sel.Tool is set here) and
// returns the job list. Every job carries the file's current size and mtime,
// which the desktop and the import compare against.
func (p *Plan) ExportJobs(ctx context.Context, db *sql.DB, sel Selection, musicDirs []string, now time.Time) (*JobsFile, ExportReport, error) {
	var rep ExportReport
	if p.group(OfflineTool) == nil {
		return nil, rep, ErrNoOfflineTool
	}
	sel.Tool = OfflineTool
	tasks, err := p.Select(ctx, db, sel)
	if err != nil {
		return nil, rep, err
	}
	f := &JobsFile{
		Format: JobsFormat, Version: JobsVersion, Created: now.UTC().Format(time.RFC3339),
		Tool: OfflineTool, Schema: folksentiaSchema, Jobs: []OfflineJob{},
	}
	var all []string
	for _, t := range tasks {
		rel, ok := RelativePath(musicDirs, t.Path)
		if !ok {
			rep.Outside++
			continue
		}
		info, err := os.Stat(t.Path)
		if err != nil || !info.Mode().IsRegular() {
			rep.Unreadable++
			continue
		}
		features := t.Runs[0].Features
		for _, ft := range features {
			if !slices.Contains(all, ft) {
				all = append(all, ft)
			}
		}
		f.Jobs = append(f.Jobs, OfflineJob{
			ID: t.SongID, Path: rel, Size: info.Size(), MtimeMs: info.ModTime().UnixMilli(),
			DurationMs: t.DurationMs, Features: features,
		})
	}
	f.Options = folksentiaOptions(all)
	rep.Jobs = len(f.Jobs)
	return f, rep, nil
}

// folksentiaOptions mirrors folksentiaArgs.
func folksentiaOptions(features []string) FolksentiaOptions {
	var o FolksentiaOptions
	o.Downbeats = slices.Contains(features, "beats")
	if slices.Contains(features, "waveform") {
		o.Waveform = folksentiaBins
	}
	o.Fingerprint = slices.Contains(features, "fingerprint")
	return o
}

// RelativePath returns p relative to the music folder that holds it, with /
// separators. The innermost folder wins when folders are nested. ok is false
// when p lies in none of them.
func RelativePath(musicDirs []string, p string) (rel string, ok bool) {
	for _, d := range musicDirs {
		r, err := filepath.Rel(d, p)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			continue
		}
		if !ok || len(r) < len(rel) {
			rel, ok = r, true
		}
	}
	return filepath.ToSlash(rel), ok
}

// resultLine is one line folksentia writes in batch mode.
type resultLine struct {
	Job         *OfflineJob     `json:"job"`
	Local       *Stamp          `json:"local"`
	ToolVersion string          `json:"toolVersion"`
	Result      json.RawMessage `json:"result"`
	Skipped     string          `json:"skipped"`
}

// ImportOptions control ImportResults.
type ImportOptions struct {
	// DryRun checks every line but stores nothing.
	DryRun bool
	// SizeOnly accepts a result when the file size matches, whatever the
	// mtime (for copies that did not keep modification times).
	SizeOnly bool
}

// ImportReport counts the lines of an import.
type ImportReport struct {
	Lines int
	// Stored results; Changed of them changed a value, Failed of them record a
	// failed run (an error from folksentia, or an impossible value).
	Stored, Changed, Failed int
	// Skipped lines, by reason: "missing" and "changed" (on the desktop, as
	// folksentia reported), "stale" (the file changed on the server since the
	// export), "unknown" (no such song), "unassigned" (none of the job's
	// features is assigned to folksentia any more), "duplicate" (a later line
	// has the same song), "invalid" (not a result line).
	Skipped map[string]int
	// Problems are short messages for invalid lines, at most maxProblems.
	Problems []string
}

const maxProblems = 20

// maxLine is the longest result line accepted: a long track's beats,
// waveform and fingerprint stay far below it.
const maxLine = 64 << 20

func (r *ImportReport) skip(reason string) { r.Skipped[reason]++ }

func (r *ImportReport) invalid(line int, msg string) {
	r.skip("invalid")
	if len(r.Problems) < maxProblems {
		r.Problems = append(r.Problems, fmt.Sprintf("line %d: %s", line, msg))
	}
}

// readLines calls fn for every non-empty line of path with its 1-based number.
func readLines(path string, fn func(n int, line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for n := 1; ; n++ {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A long line: collect it in pieces.
			buf := append([]byte(nil), line...)
			for errors.Is(err, bufio.ErrBufferFull) && len(buf) <= maxLine {
				line, err = r.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if errors.Is(err, bufio.ErrBufferFull) {
				return fmt.Errorf("line %d is longer than %d MB", n, maxLine>>20)
			}
			return err
		}
		if l := bytes.TrimSpace(line); len(l) > 0 {
			if ferr := fn(n, l); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

// ImportResults reads folksentia's result lines from path and stores each
// result exactly like a local run, after checking that the song still has
// the file the job was exported with. A bad line is counted and skipped; only
// an unreadable file or a database error stops the import.
func (p *Plan) ImportResults(ctx context.Context, db *sql.DB, path string, opt ImportOptions) (ImportReport, error) {
	rep := ImportReport{Skipped: map[string]int{}}
	g := p.group(OfflineTool)
	if g == nil {
		return rep, ErrNoOfflineTool
	}

	// First pass: the last line of each song wins, as a resumed run may have
	// written a song twice.
	last := map[string]int{}
	err := readLines(path, func(n int, line []byte) error {
		var head struct {
			Job *struct {
				ID string `json:"id"`
			} `json:"job"`
		}
		if json.Unmarshal(line, &head) == nil && head.Job != nil && head.Job.ID != "" {
			last[head.Job.ID] = n
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	err = readLines(path, func(n int, line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rep.Lines++
		var l resultLine
		if err := json.Unmarshal(line, &l); err != nil {
			rep.invalid(n, "not JSON: "+shortMessage(err.Error()))
			return nil
		}
		if l.Job == nil || l.Job.ID == "" {
			rep.invalid(n, "no job")
			return nil
		}
		if last[l.Job.ID] != n {
			rep.skip("duplicate")
			return nil
		}
		switch l.Skipped {
		case "":
		case "missing", "changed":
			rep.skip(l.Skipped)
			return nil
		default:
			rep.invalid(n, fmt.Sprintf("skipped: %q", l.Skipped))
			return nil
		}
		if len(l.Result) == 0 || string(l.Result) == "null" {
			rep.invalid(n, "no result")
			return nil
		}
		var songPath string
		err := db.QueryRowContext(ctx, "SELECT path FROM songs WHERE id = ?", l.Job.ID).Scan(&songPath)
		if errors.Is(err, sql.ErrNoRows) {
			rep.skip("unknown")
			return nil
		}
		if err != nil {
			return err
		}
		info, err := os.Stat(songPath)
		if err != nil || info.Size() != l.Job.Size || (!opt.SizeOnly && info.ModTime().UnixMilli() != l.Job.MtimeMs) {
			rep.skip("stale")
			return nil
		}
		// Only features still assigned to folksentia are stored.
		features := slices.DeleteFunc(slices.Clone(l.Job.Features), func(f string) bool { return !slices.Contains(g.Features, f) })
		if len(features) == 0 {
			rep.skip("unassigned")
			return nil
		}

		// A result in another schema is not a failed run; it is not ours to read.
		var head struct {
			Schema int `json:"schema"`
		}
		if json.Unmarshal(l.Result, &head) != nil || head.Schema != folksentiaSchema {
			rep.invalid(n, fmt.Sprintf("result schema %d, expected %d", head.Schema, folksentiaSchema))
			return nil
		}
		vals, runErr := parseFolksentia(l.Result, features)
		if runErr == nil {
			runErr = checkRanges(vals)
		}
		if runErr != nil {
			rep.Failed++
		}
		rep.Stored++
		if opt.DryRun {
			return nil
		}
		version := l.ToolVersion
		if version == "" {
			version = g.adapter.version(ctx, "")
		}
		changed, err := store(ctx, db, l.Job.ID, Run{Group: g, Features: features}, shortMessage(version), vals, runErr, stampOf(info))
		if errors.Is(err, errSongGone) {
			rep.Stored--
			rep.skip("unknown")
			return nil
		}
		if err != nil {
			return err
		}
		if changed {
			rep.Changed++
		}
		return nil
	})
	return rep, err
}

// checkRanges refuses values no track can have.
func checkRanges(vals Values) error {
	if b, ok := vals["bpm"].(*BPM); ok && (b.Value < 20 || b.Value > 300) {
		return fmt.Errorf("bpm %.1f outside 20–300", b.Value)
	}
	return nil
}
