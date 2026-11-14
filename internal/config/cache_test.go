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
