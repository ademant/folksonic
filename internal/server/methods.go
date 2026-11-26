package server

import (
	"net/http"
	"strconv"
	"strings"
)

// restMutations are the Subsonic endpoints that change state. They take GET
// as well as POST, because nearly every Subsonic client sends GET (#192):
// GET is their documented compatibility alias. They refuse HEAD, so a
// prefetcher or a link checker that only looks cannot change anything.
var restMutations = map[string]bool{
	"startScan":            true,
	"scrobble":             true,
	"updateNowPlaying":     true,
	"createPlaylist":       true,
	"updatePlaylist":       true,
	"deletePlaylist":       true,
	"star":                 true,
	"unstar":               true,
	"setRating":            true,
	"createBookmark":       true,
	"deleteBookmark":       true,
	"savePlayQueue":        true,
	"savePlayQueueByIndex": true,
}

// restAllow is the Allow value for a Subsonic endpoint.
func restAllow(name string) string {
	if restMutations[name] {
		return "GET, POST"
	}
	return "GET, HEAD, POST"
}

// restMethods answers 405 with Allow, in the Subsonic envelope, for a method
// the endpoint does not take (#192): PUT, PATCH, DELETE and the rest, and
// HEAD on an endpoint that changes state.
func restMethods(name string, h http.HandlerFunc) http.HandlerFunc {
	allow := restAllow(name)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodPost:
		case http.MethodHead:
			if restMutations[name] {
				refuseMethod(w, allow)
				return
			}
		case http.MethodOptions:
			// CORS preflight is answered before the router; a bare OPTIONS
			// gets the methods.
			w.Header().Set("Allow", allow)
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			refuseMethod(w, allow)
			return
		}
		if key, ok := badNumber(r); ok {
			writeError(w, 10, "Invalid parameter: "+key+" must be a whole number")
			return
		}
		h(w, r)
	}
}

// numericParams are the parameters the handlers read with intParam. A value
// that is not a whole number is Subsonic error 10 (#190), rather than the
// default it used to become without a word.
var numericParams = []string{
	"size", "count", "offset", "fromYear", "toYear",
	"artistCount", "artistOffset", "albumCount", "albumOffset", "songCount", "songOffset",
}

// badNumber returns the first numeric parameter that does not parse.
func badNumber(r *http.Request) (string, bool) {
	for _, k := range numericParams {
		if v := queryFirst(r, k); v != "" {
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return k, true
			}
		}
	}
	return "", false
}

func refuseMethod(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeErrorStatus(w, http.StatusMethodNotAllowed, 0, "Method not allowed; use "+strings.ReplaceAll(allow, ", ", " or "))
}

// apiMethods are the methods /api routes are registered with.
var apiMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// apiFallback answers an /api request that matches no route in JSON (#193):
// 405 with Allow when the path exists for other methods, 404 otherwise. The
// root route "/" matches every path and method, so without this a wrong
// method or an unknown /api path got the index's plain-text 404.
func apiFallback(mux *http.ServeMux) http.Handler {
	routed := func(r *http.Request, method string) bool {
		probe := r.Clone(r.Context())
		probe.Method = method
		_, pattern := mux.Handler(probe)
		return pattern != "" && pattern != "/"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || routed(r, r.Method) {
			mux.ServeHTTP(w, r)
			return
		}
		var allow []string
		for _, m := range apiMethods {
			if routed(r, m) {
				allow = append(allow, m)
				if m == http.MethodGet {
					allow = append(allow, http.MethodHead)
				}
			}
		}
		if len(allow) == 0 {
			writeAPIError(w, http.StatusNotFound, "no such endpoint")
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeAPIErrorCode(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed; use "+strings.Join(allow, " or "))
	})
}
