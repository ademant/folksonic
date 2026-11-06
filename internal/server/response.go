package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"time"
)

const (
	apiVersion = "1.16.1"
	serverType = "tinysonic"
)

// J is a shorthand for inline JSON payloads.
type J = map[string]any

func writeOK(w http.ResponseWriter, payload J) {
	if _, ok := w.(*minimalWriter); ok {
		minimise(payload)
	}
	body := J{
		"status":       "ok",
		"version":      apiVersion,
		"type":         serverType,
		"openSubsonic": true,
	}
	maps.Copy(body, payload)
	pruneNulls(body)
	writeJSON(w, http.StatusOK, J{"subsonic-response": body})
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeErrorStatus(w, http.StatusOK, code, message)
}

// writeErrorStatus answers with a Subsonic error envelope and an HTTP status.
// Most errors are HTTP 200 with the code in the body; overload and
// rate-limit answers also carry a status a proxy or client can act on.
func writeErrorStatus(w http.ResponseWriter, status, code int, message string) {
	noteError(w, code)
	body := J{
		"status":       "failed",
		"version":      apiVersion,
		"type":         serverType,
		"openSubsonic": true,
		"error":        J{"code": code, "message": message},
	}
	writeJSON(w, status, J{"subsonic-response": body})
}

// retrySeconds rounds a wait up to whole seconds for a Retry-After header.
// It is at least 1.
func retrySeconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
