package server

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// Analysis jobs and results over the API (#163). DanceSonic Studio fetches
// the job list that `tinysonic analysis export` writes, runs folksentia on
// the desktop, and uploads the result lines that `tinysonic analysis import`
// reads. Both endpoints run the CLI's code (analysis.ExportJobs and
// analysis.ImportResultsFrom); there is no second implementation.

// resultsPath is the upload route. limitBody leaves its body to the handler,
// which applies analysis_upload_max_mb instead of the global 1 MiB.
const resultsPath = "/api/analysis/results"

// maxResultLines caps the lines of one upload. A library's results are
// uploaded in batches; each call stands on its own.
const maxResultLines = 50_000

// importGate lets one results upload run at a time.
type importGate struct{ mu sync.Mutex }

// requireAnalysisImporter admits a librarian's session, or the API key a
// librarian allowed to import analysis results, sent in X-Api-Key. A key in
// the URL, a key without the grant and a device proof are refused with
// analysis_import_required; the account must be a librarian on every call.
func (s *State) requireAnalysisImporter(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	header := r.Header.Get("X-Api-Key")
	if header == "" && params(r).Get("apiKey") != "" {
		s.logRefused(r, users.CodeAnalysisImportRequired, 0, "")
		writeCoded(w, http.StatusForbidden, users.CodeAnalysisImportRequired)
		return users.User{}, false
	}
	u, ok := s.requireUser(w, r)
	if !ok {
		return u, false
	}
	switch {
	case header != "":
		allowed, err := users.KeyAllowsAnalysisImport(r.Context(), s.Pool, header)
		if err != nil {
			writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
			return users.User{}, false
		}
		if !allowed {
			s.logRefused(r, users.CodeAnalysisImportRequired, 0, "")
			writeCoded(w, http.StatusForbidden, users.CodeAnalysisImportRequired)
			return users.User{}, false
		}
	case r.Header.Get("DPoP") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "DPoP "):
		// A paired device or a device grant: not a session, and not the key.
		writeCoded(w, http.StatusForbidden, users.CodeAnalysisImportRequired)
		return users.User{}, false
	}
	if u.Role != users.RoleLibrarian {
		writeAPIErrorCode(w, http.StatusForbidden, codeLibrarianRequired, "librarians only")
		return users.User{}, false
	}
	return u, true
}

// APIAnalysisJobs returns the job list of `tinysonic analysis export`:
// features=bpm,beats,… (default: every feature assigned to folksentia) and
// exactly one of all=1, pending=1 or id=… (repeated).
func (s *State) APIAnalysisJobs(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAnalysisImporter(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	all, pending, ids := q.Get("all") == "1", q.Get("pending") == "1", q["id"]
	modes := 0
	for _, b := range []bool{all, pending, len(ids) > 0} {
		if b {
			modes++
		}
	}
	if modes != 1 {
		writeAPIError(w, http.StatusBadRequest, "give exactly one of all=1, pending=1 or id=…")
		return
	}
	plan := s.plan()
	sel := analysis.Selection{Pending: pending, IDs: ids}
	if f := q.Get("features"); f != "" {
		for name := range strings.SplitSeq(f, ",") {
			name = strings.TrimSpace(name)
			if plan.Tool(name) != analysis.OfflineTool {
				writeAPIError(w, http.StatusBadRequest, "feature "+name+" is not assigned to folksentia")
				return
			}
			sel.Features = append(sel.Features, name)
		}
	}
	jobs, rep, err := plan.ExportJobs(r.Context(), s.Pool, sel, s.MusicDirs, time.Now())
	switch {
	case errors.Is(err, analysis.ErrNoOfflineTool):
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	logging.Infof("analysis jobs: %d exported to %s (left out: %d unreadable, %d outside the music folders)",
		rep.Jobs, u.Username, rep.Unreadable, rep.Outside)
	writeJSON(w, http.StatusOK, jobs)
}

// APIAnalysisResults imports folksentia's result lines like `tinysonic
// analysis import`: dryRun=1 checks and reports without storing, sizeOnly=1
// accepts files whose size matches whatever their mtime. The body is
// application/x-ndjson, optionally with Content-Encoding: gzip; its size
// after decompression is capped by analysis_upload_max_mb.
func (s *State) APIAnalysisResults(w http.ResponseWriter, r *http.Request) {
	// Checked before anything reads the body: a form body would be parsed by
	// the credential lookup.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-ndjson" &&
		mt != "application/jsonl" && mt != "text/plain" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "send the result lines as application/x-ndjson")
		return
	}
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if enc != "" && enc != "identity" && enc != "gzip" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Content-Encoding must be gzip or absent")
		return
	}
	u, ok := s.requireAnalysisImporter(w, r)
	if !ok {
		return
	}
	if !s.imports.mu.TryLock() {
		writeAPIErrorCode(w, http.StatusConflict, codeImportRunning, "another analysis import is running")
		return
	}
	defer s.imports.mu.Unlock()

	max := s.uploadMax()
	tmp, status, err := s.spoolResults(w, r, enc == "gzip", max)
	if err != nil {
		writeAPIError(w, status, err.Error())
		return
	}
	defer os.Remove(tmp)

	q := r.URL.Query()
	opt := analysis.ImportOptions{DryRun: q.Get("dryRun") == "1", SizeOnly: q.Get("sizeOnly") == "1", MaxLines: maxResultLines}
	start := time.Now()
	rep, err := s.plan().ImportResultsFrom(r.Context(), s.Pool, func() (io.ReadCloser, error) { return os.Open(tmp) }, opt)
	switch {
	case errors.Is(err, analysis.ErrTooManyLines):
		writeAPIError(w, http.StatusRequestEntityTooLarge, err.Error()+": upload in batches")
		return
	case errors.Is(err, analysis.ErrNoOfflineTool):
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, analysis.ErrMusicUnreadable):
		logging.Warnf("analysis import by %s: %v", u.Username, err)
		writeAPIError(w, http.StatusInternalServerError, "the server cannot read the music folder")
		return
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		logging.Warnf("analysis import by %s: %v", u.Username, err)
		writeAPIError(w, http.StatusInternalServerError, "import failed")
		return
	}
	out := importReportJSON(rep, opt.DryRun)
	logging.Infof("analysis import by %s: dryRun=%t lines=%d applied=%d changed=%d failed=%d rejected=%d skipped=%d ms=%d",
		u.Username, opt.DryRun, rep.Lines, rep.Stored, rep.Changed, rep.Failed, rep.Skipped["invalid"],
		skippedTotal(rep), time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, out)
}

// uploadMax is the results upload limit in bytes.
func (s *State) uploadMax() int64 {
	mb := s.analysisUploadMaxMB
	if mb <= 0 {
		mb = 64
	}
	return int64(mb) << 20
}

// spoolResults copies the (decompressed) body to a temporary file next to
// the database, so the import can read it twice. It fails with 413 when the
// body is larger than max, before or after decompression.
func (s *State) spoolResults(w http.ResponseWriter, r *http.Request, gzipped bool, max int64) (string, int, error) {
	var body io.Reader = http.MaxBytesReader(w, r.Body, max)
	if gzipped {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return "", http.StatusBadRequest, errors.New("the body is not gzip")
		}
		defer zr.Close()
		body = zr
	}
	f, err := os.CreateTemp(s.uploadDir, ".analysis-results-*.jsonl")
	if err != nil {
		logging.Warnf("analysis import: %v", err)
		return "", http.StatusInternalServerError, errors.New("cannot store the upload")
	}
	n, err := io.Copy(f, io.LimitReader(body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	tooLarge := "the results are larger than analysis_upload_max_mb: upload in batches"
	var mbe *http.MaxBytesError
	switch {
	case n > max || errors.As(err, &mbe):
		os.Remove(f.Name())
		return "", http.StatusRequestEntityTooLarge, errors.New(tooLarge)
	case err != nil:
		os.Remove(f.Name())
		if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", http.StatusBadRequest, errors.New("the body is incomplete or not valid gzip")
		}
		return "", http.StatusBadRequest, errors.New("cannot read the body")
	}
	return f.Name(), 0, nil
}

func skippedTotal(rep analysis.ImportReport) int {
	n := 0
	for reason, c := range rep.Skipped {
		if reason != "invalid" {
			n += c
		}
	}
	return n
}

// importReportJSON is the answer of POST /api/analysis/results.
func importReportJSON(rep analysis.ImportReport, dryRun bool) J {
	skipped := map[string]int{}
	for reason, c := range rep.Skipped {
		if reason != "invalid" {
			skipped[reason] = c
		}
	}
	entries := rep.Entries
	if entries == nil {
		entries = []analysis.ImportEntry{}
	}
	changed := rep.ChangedSongs
	if changed == nil {
		changed = []analysis.ChangedSong{}
	}
	return J{
		"dryRun":       dryRun,
		"lines":        rep.Lines,
		"applied":      rep.Stored,
		"changed":      rep.Changed,
		"unchanged":    rep.Stored - rep.Changed,
		"failed":       rep.Failed,
		"skipped":      skipped,
		"rejected":     rep.Skipped["invalid"],
		"changedBy":    rep.ChangedBy,
		"entries":      entries,
		"changedSongs": changed,
	}
}

// uploadDirFor is where uploads are spooled: next to the database, which is
// on a disk the server may write to, or the system temp dir.
func uploadDirFor(databasePath string) string {
	if databasePath == "" || databasePath == ":memory:" {
		return ""
	}
	return filepath.Dir(databasePath)
}

// APIGrantAnalysisImport allows one of the caller's keys to use the analysis
// jobs and results endpoints; any other key of the account loses the grant.
// Librarians only, with a session.
func (s *State) APIGrantAnalysisImport(w http.ResponseWriter, r *http.Request) {
	s.setAnalysisImport(w, r, true)
}

// APIRemoveAnalysisImport takes the grant away from one of the caller's keys.
func (s *State) APIRemoveAnalysisImport(w http.ResponseWriter, r *http.Request) {
	s.setAnalysisImport(w, r, false)
}

func (s *State) setAnalysisImport(w http.ResponseWriter, r *http.Request, grant bool) {
	u, ok := s.requireLibrarian(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var err error
	if grant {
		err = users.GrantAnalysisImport(r.Context(), s.Pool, u.ID, id)
	} else {
		err = users.RemoveAnalysisImport(r.Context(), s.Pool, u.ID, id)
	}
	switch {
	case errors.Is(err, users.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "key not found")
		return
	case err != nil:
		writeAPIErrorCode(w, http.StatusInternalServerError, codeDatabaseError, "database error")
		return
	}
	verb := "removed from"
	if grant {
		verb = "granted to"
	}
	logging.Infof("analysis import %s api key %d of %s", verb, id, u.Username)
	writeJSON(w, http.StatusOK, J{"id": id, "analysisImport": grant})
}
