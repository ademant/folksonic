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
func (s *State) httpsGate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.httpsRequired || viaHTTPS(r, s.trusted) {
			h.ServeHTTP(w, r)
			return
		}
		s.refuseHTTP(w, r)
	})
}

// viaHTTPS reports whether the request is HTTPS, or a local tool's request.
func viaHTTPS(r *http.Request, trusted []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	peer := peerAddr(r)
	if !peer.IsValid() {
		return false
	}
	if r.Header.Get("X-Forwarded-Proto") == "https" && inPrefixes(peer, trusted) {
		return true
	}
	return peer.IsLoopback() &&
		r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Forwarded-Proto") == ""
}

// refuseHTTP answers a plain request: 426 for the API, the Subsonic envelope
// for /rest, and a redirect to the base_url address for a browser page.
func (s *State) refuseHTTP(w http.ResponseWriter, r *http.Request) {
	upgrade := func() {
		w.Header().Set("Upgrade", "TLS/1.2, HTTP/1.1")
		w.Header().Set("Connection", "Upgrade")
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/rest/"):
		upgrade()
		writeErrorStatus(w, http.StatusUpgradeRequired, 0, refusalMessage(codeHTTPSRequired))
	case strings.HasPrefix(p, "/api/"):
		upgrade()
		writeCoded(w, http.StatusUpgradeRequired, codeHTTPSRequired)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		http.Redirect(w, r, "https://"+s.httpsHost(r)+r.URL.RequestURI(), http.StatusMovedPermanently)
	default:
		upgrade()
		http.Error(w, refusalMessage(codeHTTPSRequired), http.StatusUpgradeRequired)
	}
}

// httpsHost is the host of base_url, or the Host the request arrived with.
func (s *State) httpsHost(r *http.Request) string {
	if u, err := url.Parse(s.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return r.Host
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
