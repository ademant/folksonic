package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "smolsonic.toml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnknownKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/m\"\nallow_registeration = true\n", 0o600)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "allow_registeration") {
		t.Fatalf("want unknown-key error naming the key, got %v", err)
	}
}

func TestRelativePathsResolveFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"music\"\ndatabase_path = \"data/lib.db\"\ncovers_dir = \"covers\"\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"music_dir":     cfg.MusicDir,
		"database_path": cfg.DatabasePath,
		"covers_dir":    cfg.CoversDir,
	} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, dir) {
			t.Errorf("%s = %q, want absolute path under %s", name, got, dir)
		}
	}
}

func TestAbsolutePathsUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/srv/music\"\ndatabase_path = \"/var/lib/t.db\"\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MusicDir != "/srv/music" || cfg.DatabasePath != "/var/lib/t.db" {
		t.Fatalf("absolute paths changed: %q %q", cfg.MusicDir, cfg.DatabasePath)
	}
}

func TestLooseFilePermissionsWarn(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/m\"\n", 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "chmod 600") {
		t.Fatalf("want one permission warning, got %v", cfg.Warnings)
	}
}

func TestPrivateFilePermissionsDoNotWarn(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/m\"\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestUsernameWithoutPasswordRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/m\"\nusername = \"admin\"\n", 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("want error for username without password")
	}
}
