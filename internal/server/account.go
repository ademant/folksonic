package server

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	_ "embed"

	"github.com/skip2/go-qrcode"

	"github.com/tsirysndr/tinysonic/internal/users"
)

//go:embed static/account.html
var accountPage []byte

// maxPasskeyLabel bounds the name a user gives a passkey.
const maxPasskeyLabel = 64

// ServeAccountPage serves the signed-in account page. It is static: the page
// reads the session token from sessionStorage and calls the JSON API, so the
// server does not need to know who is looking.
func (s *State) ServeAccountPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' blob:; frame-ancestors 'none'")
	_, _ = w.Write(accountPage)
}

// APIListPasskeys lists the caller's passkeys without their key material.
func (s *State) APIListPasskeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	recs, err := users.ListPasskeys(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := make([]J, 0, len(recs))
	for _, p := range recs {
		out = append(out, J{"id": p.ID, "label": p.Label, "createdAt": p.CreatedAt})
	}
	writeJSON(w, http.StatusOK, J{"passkeys": out})
}

// APIRemovePasskey deletes one of the caller's passkeys, unless it is the
// account's last way to sign in.
func (s *State) APIRemovePasskey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	recs, err := users.ListPasskeys(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !slices.ContainsFunc(recs, func(p users.PasskeyRecord) bool { return p.ID == id }) {
		writeAPIError(w, http.StatusNotFound, "passkey not found")
		return
	}
	if !s.keepsSignIn(w, r, u.ID) {
		return
	}
	if err := users.RemovePasskey(r.Context(), s.Pool, u.ID, id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, J{"status": "removed"})
}

// APIPairQR issues a pairing token and returns the QR code for it, as SVG by
// default or as PNG when the client asks for it with Accept: image/png or
// ?format=png. The token is single-use and expires after PairingTTL, like the
// token behind /api/pair/start.
func (s *State) APIPairQR(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	token, err := users.CreatePairingToken(r.Context(), s.Pool, u.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "database error")
		return
	}
	code, err := qrcode.New(PairURI(s.serverURL(r), token), qrcode.Medium)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not encode the pairing code")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if wantPNG(r) {
		png, err := code.PNG(256)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "could not render the pairing code")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	_ = writeQRSVG(w, code.Bitmap())
}

func wantPNG(r *http.Request) bool {
	if r.URL.Query().Get("format") == "png" {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "image/png") && !strings.Contains(accept, "image/svg+xml")
}

// writeQRSVG draws a QR bitmap as one path of square modules. The bitmap already
// includes the quiet zone, so the viewBox is exactly its size.
func writeQRSVG(w io.Writer, bitmap [][]bool) error {
	size := len(bitmap)
	var path strings.Builder
	for y, row := range bitmap {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	_, err := fmt.Fprintf(w,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		size, size, size, size, path.String())
	return err
}
