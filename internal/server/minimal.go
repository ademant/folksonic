package server

import (
	"net/http"
	"strings"
)

// preferMinimal is the RFC 7240 preference of the tinysonicMinimal extension.
const preferMinimal = "tinysonic-minimal"

// prefersMinimal reports whether a Prefer header asks for minimal responses.
// Other preferences, and parameters of this one, are ignored.
func prefersMinimal(r *http.Request) bool {
	for _, h := range r.Header.Values("Prefer") {
		for _, tok := range strings.Split(h, ",") {
			name, _, _ := strings.Cut(tok, ";")
			name, _, _ = strings.Cut(name, "=")
			if strings.EqualFold(strings.TrimSpace(name), preferMinimal) {
				return true
			}
		}
	}
	return false
}

// minimalWriter marks a response that writeOK should send in minimal form.
// Only JSON endpoints are wrapped; streams keep their sendfile path.
type minimalWriter struct {
	http.ResponseWriter
}

// Unwrap returns the wrapped writer, for http.ResponseController and noteError.
func (m *minimalWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// subsonicJSON is the middleware for every JSON /rest endpoint: it varies
// caches on Prefer and selects minimal mode when asked for.
func subsonicJSON(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Prefer")
		if prefersMinimal(r) {
			w.Header().Set("Preference-Applied", preferMinimal)
			h(&minimalWriter{w}, r)
			return
		}
		h(w, r)
	}
}

// Fields removed in minimal mode, by mediaType. Only optional fields that are
// constant, duplicated or derivable are listed; required fields, mediaType
// and data fields always stay. Cover art ids are derivable: an album's or
// artist's is its id, a song's is its albumId.
var minimalDrop = map[string][]string{
	"song":   {"parent", "isVideo", "type", "coverArt", "path", "suffix"},
	"album":  {"title", "coverArt"},
	"artist": {"coverArt"},
}

// minimise removes the minimalDrop fields from every object in payload.
// Folder browsing (indexes, directory) keeps a song's parent, which links the
// folder tree.
func minimise(payload J) {
	_, folder := payload["directory"]
	if _, ok := payload["indexes"]; ok {
		folder = true
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case J:
			if mt, ok := t["mediaType"].(string); ok {
				for _, f := range minimalDrop[mt] {
					if f == "parent" && folder {
						continue
					}
					delete(t, f)
				}
			}
			for _, e := range t {
				walk(e)
			}
		case []J:
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(payload)
}
