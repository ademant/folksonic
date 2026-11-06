package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMountAtServesUnderPrefix(t *testing.T) {
	s := newTestState(t)
	h := mountAt("/folksonic", s.routes())

	do := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := do("/folksonic/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "tinysonic") {
		t.Fatalf("index under prefix: status %d", rec.Code)
	}
	if rec := do("/folksonic/rest/ping"); rec.Code == http.StatusNotFound {
		t.Fatalf("subsonic endpoint under prefix not routed")
	}
	if rec := do("/folksonic/login"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `src="static/login.js"`) {
		t.Fatalf("login page under prefix: status %d", rec.Code)
	}
	if rec := do("/folksonic/static/login.js"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `post("api/login"`) {
		t.Fatalf("login script under prefix: status %d", rec.Code)
	}
	if rec := do("/folksonic"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/folksonic/" {
		t.Fatalf("bare prefix: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("/rest/ping"); rec.Code != http.StatusNotFound {
		t.Fatalf("root path reachable while mounted under prefix: status %d", rec.Code)
	}
}

func TestMountAtEmptyPrefixIsRoot(t *testing.T) {
	s := newTestState(t)
	h := mountAt("", s.routes())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login page at root: status %d", rec.Code)
	}
}
