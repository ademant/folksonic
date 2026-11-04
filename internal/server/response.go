package server

import (
	"encoding/json"
	"maps"
	"net/http"
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
	noteError(w, code)
	body := J{
		"status":       "failed",
		"version":      apiVersion,
		"type":         serverType,
		"openSubsonic": true,
		"error":        J{"code": code, "message": message},
	}
	writeJSON(w, http.StatusOK, J{"subsonic-response": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
