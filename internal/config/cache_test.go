package config

import (
	"os"
	"path/filepath"
	"testing"
)

// db_cache_mb below 1 is refused, and the default applies when it is unset (#116).
func TestDBCacheMBValidated(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "smolsonic.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write("music_dirs = [\"/music\"]\ndb_cache_mb = 0\n")); err == nil {
		t.Fatal("db_cache_mb = 0 was accepted")
	}
	c, err := Load(write("music_dirs = [\"/music\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.DBCacheMB != DefaultDBCacheMB {
		t.Fatalf("default db_cache_mb %d, want %d", c.DBCacheMB, DefaultDBCacheMB)
	}
}

// port and host are checked at load, not first at listen (#188).
func TestPortAndHostValidated(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) error {
		p := filepath.Join(dir, "smolsonic.toml")
		if err := os.WriteFile(p, []byte("music_dirs = [\"/music\"]\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		return err
	}
	for _, bad := range []string{"port = 0\n", "port = 70000\n", "port = -1\n", "host = \"bad host\"\n", "host = \"-x.example\"\n", "host = \"a..b\"\n"} {
		if load(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"", "port = 443\n", "host = \"127.0.0.1\"\n", "host = \"::1\"\n", "host = \"music.example.org\"\n", "host = \"\"\n"} {
		if err := load(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}

// site.lang and site.impressum_lang default to en and lang, and must be
// language tags (#201).
func TestSiteLanguages(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) (*Config, error) {
		p := filepath.Join(dir, "smolsonic.toml")
		if err := os.WriteFile(p, []byte("music_dirs = [\"/music\"]\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}
	c, err := load("")
	if err != nil || c.Site.Lang != "en" || c.Site.ImpressumLang != "en" {
		t.Fatalf("defaults: %+v %v", c.Site, err)
	}
	c, err = load("[site]\nlang = \"de-AT\"\n")
	if err != nil || c.Site.ImpressumLang != "de-AT" {
		t.Fatalf("impressum follows lang: %+v %v", c.Site, err)
	}
	for _, bad := range []string{"[site]\nlang = \"en us\"\n", "[site]\nimpressum_lang = \"<x>\"\n", "[site]\nlang = \"e\"\n"} {
		if _, err := load(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
