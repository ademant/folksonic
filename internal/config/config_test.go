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
	p := writeConfig(t, dir, "music_dirs = [\""+dir+"\"]\nallow_registeration = true\n", 0o600)
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
		"music_dirs[0]": cfg.MusicDirs[0],
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
	if false || cfg.DatabasePath != "/var/lib/t.db" {
		t.Fatalf("absolute paths changed: %q %q", cfg.MusicDir, cfg.DatabasePath)
	}
}

func TestLooseFilePermissionsWarn(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dirs = [\""+dir+"\"]\n", 0o644)
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
	p := writeConfig(t, dir, "music_dirs = [\""+dir+"\"]\n", 0o600)
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
	p := writeConfig(t, dir, "music_dirs = [\""+dir+"\"]\nusername = \"admin\"\n", 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("want error for username without password")
	}
}

func TestMusicDirsList(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dirs = [\"/a\", \"music\", \"/a\"]\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MusicDirs) != 2 || cfg.MusicDirs[0] != "/a" || !strings.HasPrefix(cfg.MusicDirs[1], dir) {
		t.Fatalf("MusicDirs = %v, want [/a and a path under %s] with duplicates removed", cfg.MusicDirs, dir)
	}
}

func TestMusicDirAliasStillWorks(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/srv/music\"\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MusicDirs) != 1 || cfg.MusicDirs[0] != "/srv/music" {
		t.Fatalf("MusicDirs = %v", cfg.MusicDirs)
	}
}

func TestMusicDirAndMusicDirsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dir = \"/a\"\nmusic_dirs = [\"/b\"]\n", 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("want error when both keys are set")
	}
}

func TestEmptyMusicDirsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dirs = []\n", 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("want error for empty music_dirs")
	}
}

func TestMissingMusicDirWarns(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "music_dirs = [\"/does/not/exist\"]\n", 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("missing directory must not be fatal: %v", err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "/does/not/exist") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a warning naming the missing directory, got %v", cfg.Warnings)
	}
}
