package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The API descriptions in docs/ must match the code. These tests read the
// route registrations from server.go and compare them with the documents. They
// fail when an endpoint is added without documenting it, or when a document
// lists an endpoint the server does not have.

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func serverSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOpenAPIDocumentsEveryAPIRoute compares "METHOD /api/path" pairs from the
// mux registrations with the paths and methods in docs/openapi.yaml.
func TestOpenAPIDocumentsEveryAPIRoute(t *testing.T) {
	routeRE := regexp.MustCompile(`mux\.HandleFunc\("(GET|POST|DELETE|PUT) (/api/[^"]+)"`)
	code := map[string]bool{}
	for _, m := range routeRE.FindAllStringSubmatch(serverSource(t), -1) {
		code[m[1]+" "+m[2]] = true
	}
	if len(code) == 0 {
		t.Fatal("found no /api routes in server.go; the pattern is out of date")
	}

	// The spec is parsed line by line: paths are two-space-indented keys under
	// "paths:", and operations are four-space-indented method keys below them.
	doc := map[string]bool{}
	inPaths := false
	var path string
	pathRE := regexp.MustCompile(`^  (/api/\S+):$`)
	methodRE := regexp.MustCompile(`^    (get|post|put|delete):$`)
	for _, line := range strings.Split(readDoc(t, "openapi.yaml"), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && !strings.HasPrefix(line, " ") && line != "":
			inPaths = false
		case inPaths && pathRE.MatchString(line):
			path = pathRE.FindStringSubmatch(line)[1]
		case inPaths && methodRE.MatchString(line) && path != "":
			doc[strings.ToUpper(methodRE.FindStringSubmatch(line)[1])+" "+path] = true
		}
	}

	var missing, extra []string
	for k := range code {
		if !doc[k] {
			missing = append(missing, k)
		}
	}
	for k := range doc {
		if !code[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("routes missing from docs/openapi.yaml: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("docs/openapi.yaml describes routes the server does not have: %v", extra)
	}
}

// TestOpenSubsonicListMatchesRegistrations checks that the "Implemented" list
// in docs/opensubsonic-endpoints.md is exactly the set of registered Subsonic
// endpoints, and that the "Not implemented" list names none of them.
func TestOpenSubsonicListMatchesRegistrations(t *testing.T) {
	regRE := regexp.MustCompile(`register(?:Raw)?\("([A-Za-z0-9]+)"`)
	registered := map[string]bool{}
	for _, m := range regRE.FindAllStringSubmatch(serverSource(t), -1) {
		registered[m[1]] = true
	}

	if len(registered) == 0 {
		t.Fatal("found no register(...) calls in server.go; the pattern is out of date")
	}
	doc := readDoc(t, "opensubsonic-endpoints.md")
	implemented, notImplemented := map[string]bool{}, map[string]bool{}
	section := ""
	nameRE := regexp.MustCompile("`([A-Za-z0-9]+)`")
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "## Implemented"):
			section = "impl"
		case strings.HasPrefix(line, "## Not implemented"):
			section = "not"
		case strings.HasPrefix(line, "## "):
			section = ""
		case section == "impl" || section == "not":
			for _, m := range nameRE.FindAllStringSubmatch(line, -1) {
				if section == "impl" {
					implemented[m[1]] = true
				} else {
					notImplemented[m[1]] = true
				}
			}
		}
	}

	if got, want := sortedKeys(implemented), sortedKeys(registered); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Implemented list does not match registrations.\nlisted but not registered: %v\nregistered but not listed: %v",
			diff(got, registered), diff(want, implemented))
	}
	for name := range notImplemented {
		if registered[name] {
			t.Errorf("%s is listed as not implemented but is registered", name)
		}
	}
	if len(notImplemented) == 0 {
		t.Error("Not implemented list is empty")
	}
}

// diff returns the names in a that are not in the set b.
func diff(a []string, b map[string]bool) []string {
	var out []string
	for _, n := range a {
		if !b[n] {
			out = append(out, n)
		}
	}
	return out
}

// rfcErrorResponses keep their own error shape: RFC 8628 for the device flow,
// plain text for the OIDC callback.
var rfcErrorResponses = map[string]bool{
	"POST /api/device/token 400":   true,
	"POST /api/device/refresh 400": true,
	"GET /api/oidc/callback 400":   true,
}

// TestOpenAPIErrorsUseTheErrorSchema checks the one error contract (#194):
// the Error schema requires both error and message, and every 4xx/5xx
// response that declares a body uses it.
func TestOpenAPIErrorsUseTheErrorSchema(t *testing.T) {
	doc := readDoc(t, "openapi.yaml")
	if !regexp.MustCompile(`(?m)^    Error:\n(?:      .*\n)*?      required: \[error, message\]$`).MatchString(doc) {
		t.Error("the Error schema does not require error and message")
	}
	lines := strings.Split(doc, "\n")
	pathRE := regexp.MustCompile(`^  (/api/\S+):$`)
	methodRE := regexp.MustCompile(`^    (get|post|put|delete):$`)
	statusRE := regexp.MustCompile(`^        '([45]\d\d)':$`)
	var path, method string
	checked := 0
	for i, line := range lines {
		if m := pathRE.FindStringSubmatch(line); m != nil {
			path = m[1]
		}
		if m := methodRE.FindStringSubmatch(line); m != nil {
			method = strings.ToUpper(m[1])
		}
		m := statusRE.FindStringSubmatch(line)
		if m == nil || path == "" {
			continue
		}
		var block []string
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(l, "          ") {
				break
			}
			block = append(block, l)
		}
		b := strings.Join(block, "\n")
		key := method + " " + path + " " + m[1]
		if !strings.Contains(b, "content:") || rfcErrorResponses[key] {
			continue
		}
		checked++
		if !strings.Contains(b, "#/components/schemas/Error'") {
			t.Errorf("%s: error response body is not the Error schema", key)
		}
	}
	if checked == 0 {
		t.Fatal("found no error responses with a body; the pattern is out of date")
	}
}

// TestJSONErrorsHaveOneShape keeps every JSON error going through
// writeAPIErrorCode or writeCoded, which write {"error", "message"} (#194).
// Only the RFC 8628 device flow (grant.go) writes its own shape.
func TestJSONErrorsHaveOneShape(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]int{"api_users.go": 1, "refusal.go": 1, "grant.go": 1}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), `J{"error":`); n > allowed[f] {
			t.Errorf("%s writes %d JSON error bodies of its own; use writeAPIError or writeCoded", f, n)
		}
	}

	rec := httptest.NewRecorder()
	writeAPIError(rec, http.StatusNotFound, "song not found")
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["error"] != "not_found" || out["message"] != "song not found" {
		t.Fatalf("writeAPIError: %v %s", err, rec.Body)
	}
	for status := range statusCodes {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(statusCode(status)) {
			t.Errorf("status %d: code %q", status, statusCode(status))
		}
	}
}
