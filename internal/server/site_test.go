package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func getPage(t *testing.T, h http.HandlerFunc, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestInfoPageForBrowsersBannerForOthers(t *testing.T) {
	s := newTestState(t)
	s.Site.Name = `<script>alert(1)</script>`
	s.Site.Description = "Our folk library."
	s.Site.Contact = "ops@example.org"
	s.Site.Repository = "https://github.com/ademant/folksonic"

	rec := getPage(t, s.handleIndex, "/", "text/html,application/xhtml+xml")
	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("browser got %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("site name not escaped")
	}
	for _, want := range []string{
		"Our folk library.",
		"mailto:ops@example.org",
		"https://github.com/ademant/folksonic/blob/main/docs/openapi.yaml",
		"https://github.com/ademant/folksonic/blob/main/docs/opensubsonic-endpoints.md",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("info page lacks %q", want)
		}
	}
	if strings.Contains(body, "Impressum") {
		t.Error("Impressum link shown without a file")
	}

	curl := getPage(t, s.handleIndex, "/", "*/*")
	if !strings.Contains(curl.Body.String(), "tinysonic v") {
		t.Errorf("non-browser client did not get the banner: %q", curl.Body.String()[:40])
	}
}

func TestImpressumRendersMarkdownAndEscapesHTML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "impressum.md")
	src := "# Impressum\n\nAnna Beispiel, Musterweg 1.\n\n<b>raw</b> text\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	html, ok := renderImpressum(path)
	if !ok {
		t.Fatal("impressum not loaded")
	}
	s := newTestState(t)
	s.impressumHTML, s.impressumOK = html, ok

	rec := getPage(t, s.ServeImpressum, "/impressum", "")
	body := rec.Body.String()
	if !strings.Contains(body, "<h1>Impressum</h1>") || !strings.Contains(body, "Musterweg 1.") {
		t.Fatalf("markdown not rendered: %s", body)
	}
	if strings.Contains(body, "<b>raw</b>") {
		t.Error("raw HTML from the Markdown file reached the page")
	}
}

func TestImpressumNotFoundWithoutFile(t *testing.T) {
	s := newTestState(t)
	if rec := getPage(t, s.ServeImpressum, "/impressum", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d without impressum_file, want 404", rec.Code)
	}
}

func TestRobotsKeepsAPIOutAndNamesSitemap(t *testing.T) {
	s := newTestState(t)
	s.BasePath = "/folksonic"
	s.BaseURL = "https://music.example.org/folksonic"

	body := getPage(t, s.ServeRobots, "/robots.txt", "").Body.String()
	for _, want := range []string{
		"Disallow: /folksonic/rest/",
		"Disallow: /folksonic/api/",
		"Disallow: /folksonic/login",
		"Disallow: /folksonic/library",
		"Sitemap: https://music.example.org/folksonic/sitemap.xml",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("robots.txt lacks %q:\n%s", want, body)
		}
	}
}

func TestSitemapListsPublicPagesWithBaseURL(t *testing.T) {
	s := newTestState(t)
	if rec := getPage(t, s.ServeSitemap, "/sitemap.xml", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("sitemap without base_url: status %d, want 404", rec.Code)
	}

	s.BaseURL = "https://music.example.org/"
	s.impressumOK = true
	body := getPage(t, s.ServeSitemap, "/sitemap.xml", "").Body.String()
	if !strings.Contains(body, "<loc>https://music.example.org/</loc>") ||
		!strings.Contains(body, "<loc>https://music.example.org/impressum</loc>") {
		t.Fatalf("sitemap entries wrong:\n%s", body)
	}
	if strings.Contains(body, "/library") || strings.Contains(body, "/login") {
		t.Error("sitemap lists a private page")
	}

	s.impressumOK = false
	body = getPage(t, s.ServeSitemap, "/sitemap.xml", "").Body.String()
	if strings.Contains(body, "impressum") {
		t.Error("sitemap lists a missing Impressum")
	}
}

func TestSignInAndLibraryPagesAskNotToBeIndexed(t *testing.T) {
	s := newTestState(t)
	for name, h := range map[string]http.HandlerFunc{
		"login":   s.ServeLoginPage,
		"account": s.ServeAccountPage,
		"library": s.ServeLibraryPage,
	} {
		if got := getPage(t, h, "/", "").Header().Get("X-Robots-Tag"); got != "noindex" {
			t.Errorf("%s page X-Robots-Tag = %q", name, got)
		}
	}
}
