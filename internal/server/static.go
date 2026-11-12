package server

import (
	"embed"
	"net/http"
	"path"
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
// audio streams, which are never framed either. The referrer is not sent to
// other sites (the URL of a page can hold a key), and no page may use the
// sensor and device features; WebAuthn is not affected by that policy.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.ServeHTTP(w, r)
	})
}

// cacheDefaults sets Cache-Control on the Subsonic and API responses. Personal
// responses are not stored at all; audio and covers are private, so the
// client may keep them but no shared cache may. A handler that sets its own
// value replaces this default, because it runs after it.
func cacheDefaults(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rest/") || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", cacheControlFor(r.URL.Path))
		}
		h.ServeHTTP(w, r)
	})
}

// cacheControlFor is the Cache-Control value for a Subsonic or API path.
func cacheControlFor(p string) string {
	switch strings.TrimSuffix(path.Base(p), ".view") {
	case "stream", "download":
		return "private, max-age=3600"
	case "getCoverArt":
		return "private, max-age=86400"
	}
	return "no-store"
}
