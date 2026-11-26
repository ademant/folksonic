package server

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// / varies by Accept, and the public pages carry canonical, OpenGraph, icon
// and the configured language when base_url is set (#201).
func TestPublicPageMetadata(t *testing.T) {
	s := newTestState(t)
	s.BaseURL = "https://example.org/folksonic"
	s.BasePath = "/folksonic"
	s.Site = config.Site{Name: "Folk Cloud", Description: "Dance music.", Lang: "en", ImpressumLang: "de"}
	s.impressumHTML, s.impressumOK = template.HTML("<p>Angaben</p>"), true
	h := s.routes()

	for _, accept := range []string{"text/html", ""} {
		if v := getThrough(t, h, "/", accept).Header().Get("Vary"); v != "Accept" {
			t.Errorf("Accept %q: Vary %q", accept, v)
		}
	}
	page := getThrough(t, h, "/", "text/html").Body.String()
	for _, want := range []string{
		`<html lang="en">`,
		`<link rel="canonical" href="https://example.org/folksonic/">`,
		`<meta property="og:title" content="Folk Cloud">`,
		`<meta property="og:description" content="Dance music.">`,
		`<link rel="icon" href="/folksonic/static/favicon.svg"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("info page lacks %s", want)
		}
	}
	imp := getThrough(t, h, "/impressum", "").Body.String()
	for _, want := range []string{`<html lang="de">`, `href="https://example.org/folksonic/impressum"`, `<title>Impressum – Folk Cloud</title>`} {
		if !strings.Contains(imp, want) {
			t.Errorf("impressum lacks %s", want)
		}
	}

	// Without base_url there is no address to be canonical about.
	s.BaseURL = ""
	if page := getThrough(t, s.routes(), "/", "text/html").Body.String(); strings.Contains(page, "canonical") || strings.Contains(page, "og:") {
		t.Error("canonical or OpenGraph without base_url")
	}
}

// Static files revalidate with their ETag, and the icon is served (#201).
func TestStaticFilesHaveETags(t *testing.T) {
	h := newTestState(t).routes()
	rec := getThrough(t, h, "/static/site.css", "")
	tag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || tag == "" {
		t.Fatalf("site.css: %d, ETag %q", rec.Code, tag)
	}
	req := httptest.NewRequest(http.MethodGet, "/static/site.css", nil)
	req.Header.Set("If-None-Match", tag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation: %d", rec.Code)
	}
	if rec := getThrough(t, h, "/static/favicon.svg", ""); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
