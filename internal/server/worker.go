package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/logging"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// The analysis worker endpoints (#78). A worker runs a tool on its own machine
// and talks to these endpoints with a worker key (see users.ScopeWorker). The
// key goes in the X-Api-Key header; the query string is refused, because
// proxies log it.

// workerResultLimit caps the body of a result: a folksentia output with
// waveform and beats is a few hundred kilobytes.
const workerResultLimit = 32 << 20

// apiKeyOf returns the API key a request presents, and whether it came in the
// X-Api-Key header. The apiKey query parameter is what Subsonic clients send.
func apiKeyOf(r *http.Request) (key string, fromHeader bool) {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k, true
	}
	return params(r).Get("apiKey"), false
}

// workerCaller is the worker key that made a worker request.
type workerCaller struct {
	User  users.User
	KeyID int64
}

// requireWorker admits only worker keys in the X-Api-Key header.
func (s *State) requireWorker(w http.ResponseWriter, r *http.Request) (workerCaller, bool) {
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		s.writeRefusal(w, r)
		return workerCaller{}, false
	}
	u, info, err := users.LookupAPIKeyInfo(r.Context(), s.Pool, key)
	if err != nil {
		reason, keyID, label := users.APIKeyProblem(r.Context(), s.Pool, key)
		s.logRefused(r, reason, keyID, label)
		s.writeRefusal(w, r)
		return workerCaller{}, false
	}
	if info.Scope != users.ScopeWorker {
		writeCoded(w, http.StatusForbidden, users.CodeScopeForbidden)
		return workerCaller{}, false
	}
	return workerCaller{User: u, KeyID: info.ID}, true
}

// APIWorkerJobs gives the calling worker up to `limit` tracks for a tool that
// the server assigns to workers. Each track is leased for analysis.WorkerLease.
// Paths are relative to the music directory, as everywhere in the API.
func (s *State) APIWorkerJobs(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.requireWorker(w, r)
	if !ok {
		return
	}
	var body struct {
		Tool  string `json:"tool"`
		Limit int    `json:"limit"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Tool == "" {
		writeAPIError(w, http.StatusBadRequest, `"tool" is required`)
		return
	}
	if body.Limit == 0 {
		body.Limit = 5
	}
	if body.Limit < 1 || body.Limit > 20 {
		writeAPIError(w, http.StatusBadRequest, "limit must be between 1 and 20")
		return
	}
	jobs, err := s.plan().LeaseWorkerJobs(r.Context(), s.Pool, body.Tool, wk.KeyID, body.Limit, time.Now())
	if errors.Is(err, analysis.ErrNotWorkerTool) {
		writeAPIError(w, http.StatusBadRequest, "tool is not served by analysis workers")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := []J{}
	for _, j := range jobs {
		out = append(out, J{
			"id": j.SongID, "path": relativePath(s.MusicDirs, j.Path),
			"size": j.Stamp.Size, "mtimeMs": j.Stamp.MtimeMs,
			"durationMs": j.DurationMs, "features": j.Features,
		})
	}
	logging.Infof("worker %s: %d jobs for %s", wk.User.Username, len(out), body.Tool)
	writeJSON(w, http.StatusOK, J{"jobs": out, "leaseSeconds": int(analysis.WorkerLease.Seconds())})
}

// APIWorkerResults stores a worker's answer for a track it holds a lease on.
// `output` is the tool's stdout; `error` is set instead when the tool could
// not run. `size` and `mtimeMs` are the fingerprint the job was given out with.
func (s *State) APIWorkerResults(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.requireWorker(w, r)
	if !ok {
		return
	}
	var body struct {
		ID      string `json:"id"`
		Tool    string `json:"tool"`
		Size    int64  `json:"size"`
		MtimeMs int64  `json:"mtimeMs"`
		Output  string `json:"output"`
		Error   string `json:"error"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.ID == "" || body.Tool == "" || (body.Output == "" && body.Error == "") {
		writeAPIError(w, http.StatusBadRequest, `"id", "tool" and "output" or "error" are required`)
		return
	}
	changed, err := s.plan().StoreWorkerResult(r.Context(), s.Pool, analysis.WorkerResult{
		SongID: body.ID, Tool: body.Tool, KeyID: wk.KeyID,
		Stamp:  analysis.Stamp{Size: body.Size, MtimeMs: body.MtimeMs},
		Output: []byte(body.Output), Error: strings.TrimSpace(body.Error),
	})
	switch {
	case errors.Is(err, analysis.ErrNotWorkerTool):
		writeAPIError(w, http.StatusBadRequest, "tool is not served by analysis workers")
	case errors.Is(err, analysis.ErrLeaseNotHeld):
		writeAPIError(w, http.StatusConflict, "the track is not leased to this worker; it may have expired and been given to another worker")
	case errors.Is(err, analysis.ErrFileChanged):
		writeAPIError(w, http.StatusConflict, "the file changed after the job was given out; the next scan picks it up")
	case err != nil:
		logging.Warnf("worker %s: storing result for %s: %v", wk.User.Username, body.ID, err)
		writeAPIError(w, http.StatusInternalServerError, "database error")
	default:
		writeJSON(w, http.StatusOK, J{"status": "stored", "changed": changed})
	}
}
