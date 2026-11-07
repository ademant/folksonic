package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// analysisRetryAfter is the Retry-After (seconds) of a 202 response.
const analysisRetryAfter = 30

// APIAnalysis serves the stored analysis of one song: GET /api/analysis?id=.
// It only reads the database; analysis itself runs in background jobs.
//
// Every feature is null when unknown. 404: no such song. 202 with
// Retry-After: automatic analysis is on and some assigned features have no
// result yet; the body has what is known and lists the rest in "pending".
func (s *State) APIAnalysis(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	var path string
	var size, mtimeSec int64
	var fileSize, fileMtimeMs sql.NullInt64
	var bpm, bpmConf sql.NullFloat64
	err := s.Pool.QueryRowContext(ctx,
		"SELECT path, filesize, mtime, file_size, file_mtime_ms, bpm, bpm_confidence FROM songs WHERE id = ?", id).
		Scan(&path, &size, &mtimeSec, &fileSize, &fileMtimeMs, &bpm, &bpmConf)
	if errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, http.StatusNotFound, "song not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}

	out := J{"id": id}
	for _, f := range []string{"waveform", "beats", "bpm", "key", "loudness", "silence", "fingerprint"} {
		out[f] = nil
	}
	// fileVersion names the file the values belong to. It changes when the
	// file does; the path is hashed, not exposed.
	mtimeMs := mtimeSec * 1000
	if fileSize.Valid && fileMtimeMs.Valid {
		size, mtimeMs = fileSize.Int64, fileMtimeMs.Int64
	}
	sum := sha256.Sum256([]byte(path + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(mtimeMs, 10)))
	out["fileVersion"] = "sha256:" + hex.EncodeToString(sum[:])
	if bpm.Valid {
		out["bpm"] = J{"value": bpm.Float64, "confidence": nfToAny(bpmConf)}
	}

	rows, err := s.Pool.QueryContext(ctx, "SELECT name, value FROM song_values WHERE song_id = ?", id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "database error")
			return
		}
		if _, known := out[name]; known && name != "bpm" && json.Valid([]byte(value)) {
			out[name] = json.RawMessage(value)
		}
	}
	if err := rows.Err(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}

	p := s.plan()
	if p.Auto {
		pending, err := p.PendingFeatures(ctx, s.Pool, id)
		if err != nil {
			logging.Errorf("analysis %s: pending features: %v", id, err)
		}
		if len(pending) > 0 {
			out["pending"] = pending
			w.Header().Set("Retry-After", strconv.Itoa(analysisRetryAfter))
			writeJSON(w, http.StatusAccepted, out)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func nfToAny(f sql.NullFloat64) any {
	if f.Valid {
		return f.Float64
	}
	return nil
}
