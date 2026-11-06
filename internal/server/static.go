package server

import (
	"embed"
	"net/http"
	"path/filepath"
	"strings"
)

// The web pages load their stylesheets and scripts from these files rather
// than inline, so the Content-Security-Policy can refuse inline code. Only
// .css and .js files in this directory are served.
//
//go:embed static/*.css static/*.js
var staticFiles embed.FS

// ServeStatic serves a stylesheet or script from the embedded static files.
func (s *State) ServeStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var ctype string
	switch strings.ToLower(filepath.Ext(name)) {
	case ".css":
		ctype = "text/css; charset=utf-8"
	case ".js":
		ctype = "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// securityHeaders stops other sites from framing tinysonic and stops browsers
// from guessing a response's type. Every response gets them, including the
// audio streams, which are never framed either.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}
