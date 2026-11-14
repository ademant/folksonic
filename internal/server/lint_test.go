package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #110: a repository read whose error is ignored answers an empty list as if
// it were the whole result. This test fails on any `x, _ := repo…` or
// `if x, _ := repo…` in the package, so the mistake cannot come back.
func TestRepositoryErrorsAreNotIgnored(t *testing.T) {
	ignored := regexp.MustCompile(`,\s*_\s*:?=\s*repo[A-Z]\w*\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if ignored.MatchString(line) {
				t.Errorf("%s:%d ignores a repository error: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
