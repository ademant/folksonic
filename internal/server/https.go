package server

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// httpsGate refuses plain HTTP on every endpoint when require_https is on. A
// request counts as HTTPS if it arrived over TLS, or if a trusted reverse proxy
// says X-Forwarded-Proto: https. A local tool on the server (loopback, with no
// proxy headers, as the deploy health check sends) is allowed; a request that a
// proxy passed on carries X-Forwarded-For, so it is not local.
//
// With require_https on, an HTTPS response carries Strict-Transport-Security
// (#180), so a browser that has been here once never tries plain HTTP again.
func (s *State) httpsGate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.httpsRequired {
			h.ServeHTTP(w, r)
			return
		}
		if isHTTPS(r, s.trusted) {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		if viaHTTPS(r, s.trusted) {
			h.ServeHTTP(w, r)
			return
		}
		s.refuseHTTP(w, r)
	})
}

// hstsValue is a year, without includeSubDomains: the server may share its
// host name's parent with sites it knows nothing about.
const hstsValue = "max-age=31536000"

// isHTTPS reports whether the request came over HTTPS: TLS here, or a
// trusted proxy's X-Forwarded-Proto: https.
func isHTTPS(r *http.Request, trusted []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	peer := peerAddr(r)
	return peer.IsValid() && r.Header.Get("X-Forwarded-Proto") == "https" && inPrefixes(peer, trusted)
}

// viaHTTPS reports whether the request is HTTPS, or a local tool's request.
func viaHTTPS(r *http.Request, trusted []netip.Prefix) bool {
	if isHTTPS(r, trusted) {
		return true
	}
	peer := peerAddr(r)
	return peer.IsValid() && peer.IsLoopback() &&
		r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Forwarded-Proto") == ""
}

// refuseHTTP answers a plain request: 426 for the API, the Subsonic envelope
// for /rest, and a redirect to the base_url address for a browser page. The
// path is classified below base_path, since the gate runs before the prefix
// is stripped (#191).
func (s *State) refuseHTTP(w http.ResponseWriter, r *http.Request) {
	upgrade := func() {
		w.Header().Set("Upgrade", "TLS/1.2, HTTP/1.1")
		w.Header().Set("Connection", "Upgrade")
	}
	p := r.URL.Path
	if s.BasePath != "" {
		if rest, ok := strings.CutPrefix(p, s.BasePath); ok && (rest == "" || rest[0] == '/') {
			p = rest
		}
	}
	switch {
	case strings.HasPrefix(p, "/rest/"):
		upgrade()
		writeErrorStatus(w, http.StatusUpgradeRequired, 0, refusalMessage(codeHTTPSRequired))
	case strings.HasPrefix(p, "/api/"):
		upgrade()
		writeCoded(w, http.StatusUpgradeRequired, codeHTTPSRequired)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		// Only to the configured address, never the client's Host header,
		// and without credentials in the query (#191).
		origin, ok := s.httpsOrigin()
		if !ok {
			http.Error(w, refusalMessage(codeHTTPSRequired), http.StatusForbidden)
			return
		}
		target := origin + r.URL.EscapedPath()
		if q := r.URL.Query(); len(q) > 0 {
			raw := r.URL.RawQuery
			if scrubCredentials(q) {
				raw = q.Encode()
			}
			if raw != "" {
				target += "?" + raw
			}
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	default:
		upgrade()
		http.Error(w, refusalMessage(codeHTTPSRequired), http.StatusUpgradeRequired)
	}
}

// httpsOrigin is "https://host" of base_url, when base_url is an https
// address.
func (s *State) httpsOrigin() (string, bool) {
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return "https://" + u.Host, true
}

// credentialParams are query parameters that carry a secret: Subsonic
// credentials, API keys, signed stream links, invitation and pairing tokens.
var credentialParams = []string{"u", "p", "t", "s", "apiKey", "sig", "uid", "exp", "invite", "token", "code"}

// scrubCredentials removes credentialParams from q, so a redirect does not
// copy them into the Location and the next page's Referer. It reports
// whether it removed any.
func scrubCredentials(q url.Values) bool {
	removed := false
	for _, k := range credentialParams {
		if q.Has(k) {
			q.Del(k)
			removed = true
		}
	}
	return removed
}

// peerAddr is the address the connection came from, or an invalid address.
func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
