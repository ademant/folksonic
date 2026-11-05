package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSiteConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "smolsonic.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSiteDefaultsAndImpressumPath(t *testing.T) {
	p := writeSiteConfig(t, `music_dirs = ["music"]
[site]
name = "Folk Cloud"
impressum_file = "legal/impressum.md"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Site.Repository != DefaultRepository {
		t.Errorf("repository = %q, want the default", c.Site.Repository)
	}
	want := filepath.Join(filepath.Dir(p), "legal", "impressum.md")
	if c.Site.ImpressumFile != want {
		t.Errorf("impressum_file = %q, want %q", c.Site.ImpressumFile, want)
	}
}

func TestSiteRepositoryMustBeHTTP(t *testing.T) {
	p := writeSiteConfig(t, `music_dirs = ["music"]
[site]
repository = "git@github.com:ademant/folksonic.git"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "site.repository") {
		t.Fatalf("ssh repository accepted: %v", err)
	}
}
