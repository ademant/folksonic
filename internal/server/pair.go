package server

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed static/pair.html
var pairPage []byte

// PairLink is the pairing as an HTTPS link: the same token as the QR code, in
// the URL fragment. Browsers never send a fragment to the server, so the token
// stays out of access logs and Referer headers (#129).
func PairLink(server, token string) string {
	return strings.TrimSuffix(server, "/") + "/pair#token=" + token
}

// ServePairPage serves the page that holds a pairing link, with a copy button.
// It does not complete the pairing: that stays POST /api/pair/complete, which
// still needs the verification code on both sides.
func (s *State) ServePairPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
	_, _ = w.Write(pairPage)
}
