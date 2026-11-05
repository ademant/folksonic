package server

import (
	"bytes"
	"encoding/xml"
	"html/template"
	"net/http"
	"os"
	"strings"

	"github.com/yuin/goldmark"

	"github.com/tsirysndr/tinysonic/internal/cli"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

// The public pages: the information page at /, the Impressum, robots.txt and
// sitemap.xml. They describe the server (config [site]) and never expose the
// library. Paths under base_path are prefixed with the mount point, because
// that is where the server answers.

// sitePageCSP allows the inline style of the public pages and nothing else.
const sitePageCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; form-action 'none'"

// siteStyle is shared by the information page and the Impressum page.
const siteStyle = `
body { font: 16px/1.5 system-ui, sans-serif; margin: 0; background: #fbfbfc; color: #1d1f24; }
main { max-width: 42rem; margin: 0 auto; padding: 2rem 1rem; }
h1 { font-size: 1.8rem; margin: 0 0 .25rem; }
.version { color: #6b7080; font-size: .9rem; margin: 0 0 1.5rem; }
a { color: #2456c8; }
nav ul { padding-left: 1.2rem; }
footer { margin-top: 3rem; color: #6b7080; font-size: .85rem; }
@media (prefers-color-scheme: dark) {
  body { background: #15161a; color: #e6e7eb; }
  a { color: #8fb4ff; }
  .version, footer { color: #9499a8; }
}`

var infoTemplate = template.Must(template.New("info").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="description" content="{{.Description}}">
<title>{{.Name}}</title>
<style>` + siteStyle + `</style>
</head>
<body>
<main>
<h1>{{.Name}}</h1>
<p class="version">tinysonic {{.Version}}</p>
{{if .Description}}<p>{{.Description}}</p>{{end}}
<nav>
<ul>
{{if .Contact}}<li>Contact: {{if .ContactHref}}<a href="{{.ContactHref}}">{{.Contact}}</a>{{else}}{{.Contact}}{{end}}</li>{{end}}
<li><a href="{{.Repository}}">Source code on GitHub</a></li>
<li><a href="{{.OpenAPI}}">API description (OpenAPI)</a></li>
<li><a href="{{.Endpoints}}">Supported Subsonic and OpenSubsonic endpoints</a></li>
{{if .Impressum}}<li><a href="{{.Impressum}}">Impressum</a></li>{{end}}
<li><a href="{{.Login}}">Sign in</a></li>
</ul>
</nav>
<footer>Subsonic-compatible music server. Clients use the <code>/rest</code> API.</footer>
</main>
</body>
</html>
`))

var impressumTemplate = template.Must(template.New("impressum").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Impressum</title>
<style>` + siteStyle + `</style>
</head>
<body>
<main>
{{.Body}}
<footer><a href="{{.Home}}">Back</a></footer>
</main>
</body>
</html>
`))

// siteURL joins the mount point and a path, such as "/impressum".
func (s *State) siteURL(p string) string {
	return s.BasePath + p
}

// renderImpressum converts the configured Markdown to HTML once at startup.
// goldmark does not pass raw HTML through, so the file cannot inject markup.
func renderImpressum(path string) (template.HTML, bool) {
	if path == "" {
		return "", false
	}
	src, err := os.ReadFile(path)
	if err != nil {
		logging.Warnf("impressum_file: %v; the Impressum page is disabled", err)
		return "", false
	}
	var buf bytes.Buffer
	if err := goldmark.Convert(src, &buf); err != nil {
		logging.Warnf("impressum_file: %v; the Impressum page is disabled", err)
		return "", false
	}
	return template.HTML(buf.String()), true
}

// serveInfoPage is the HTML information page at /. handleIndex calls it for
// browsers; other clients keep the plain-text banner.
func (s *State) serveInfoPage(w http.ResponseWriter, r *http.Request) {
	site := s.Site
	name := site.Name
	if name == "" {
		name = "tinysonic"
	}
	repo := strings.TrimSuffix(site.Repository, "/")
	contactHref := ""
	switch {
	case strings.HasPrefix(site.Contact, "http://"), strings.HasPrefix(site.Contact, "https://"):
		contactHref = site.Contact
	case strings.Contains(site.Contact, "@") && !strings.Contains(site.Contact, " "):
		contactHref = "mailto:" + site.Contact
	}
	impressum := ""
	if s.impressumOK {
		impressum = s.siteURL("/impressum")
	}
	var buf bytes.Buffer
	err := infoTemplate.Execute(&buf, map[string]any{
		"Name":        name,
		"Description": site.Description,
		"Contact":     site.Contact,
		"ContactHref": contactHref,
		"Version":     cli.Version,
		"Repository":  repo,
		"OpenAPI":     repo + "/blob/main/docs/openapi.yaml",
		"Endpoints":   repo + "/blob/main/docs/opensubsonic-endpoints.md",
		"Impressum":   impressum,
		"Login":       s.siteURL("/login"),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", sitePageCSP)
	_, _ = w.Write(buf.Bytes())
}

// acceptsHTML reports whether the client asked for a web page.
func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ServeImpressum serves the legal notice from impressum_file. Without one, it
// is not found.
func (s *State) ServeImpressum(w http.ResponseWriter, r *http.Request) {
	if !s.impressumOK {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	err := impressumTemplate.Execute(&buf, map[string]any{
		"Body": s.impressumHTML,
		"Home": s.siteURL("/"),
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", sitePageCSP)
	_, _ = w.Write(buf.Bytes())
}

// ServeRobots disallows the API and the sign-in and library pages, and points
// crawlers at the sitemap when base_url is set.
func (s *State) ServeRobots(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for _, p := range []string{"/rest/", "/api/", "/login", "/account", "/library"} {
		b.WriteString("Disallow: " + s.BasePath + p + "\n")
	}
	if s.BaseURL != "" {
		b.WriteString("\nSitemap: " + siteRoot(s.BaseURL) + "sitemap.xml\n")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// ServeSitemap lists the public pages with absolute URLs built from base_url.
// It is not found when base_url is not set, because a guessed host would be
// wrong.
func (s *State) ServeSitemap(w http.ResponseWriter, r *http.Request) {
	if s.BaseURL == "" {
		http.NotFound(w, r)
		return
	}
	type urlEntry struct {
		Loc string `xml:"loc"`
	}
	type urlset struct {
		XMLName xml.Name   `xml:"urlset"`
		Xmlns   string     `xml:"xmlns,attr"`
		URLs    []urlEntry `xml:"url"`
	}
	set := urlset{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs, urlEntry{Loc: siteRoot(s.BaseURL)})
	if s.impressumOK {
		set.URLs = append(set.URLs, urlEntry{Loc: siteRoot(s.BaseURL) + "impressum"})
	}
	out, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(append([]byte(xml.Header), out...))
}

// siteRoot returns base_url with exactly one trailing slash.
func siteRoot(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/") + "/"
}
