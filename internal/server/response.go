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
	body := J{
		"status":  "ok",
		"version": apiVersion,
		"type":    serverType,
	}
	maps.Copy(body, payload)
	writeJSON(w, http.StatusOK, J{"subsonic-response": body})
}

func writeError(w http.ResponseWriter, code int, message string) {
	body := J{
		"status":  "failed",
		"version": apiVersion,
		"type":    serverType,
		"error":   J{"code": code, "message": message},
	}
	writeJSON(w, http.StatusOK, J{"subsonic-response": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
