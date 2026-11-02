package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	MusicDir          string `toml:"music_dir"`
	Username          string `toml:"username"`
	Password          string `toml:"password"`
	Port              int    `toml:"port"`
	Host              string `toml:"host"`
	DatabasePath      string `toml:"database_path"`
	CoversDir         string `toml:"covers_dir"`
	BaseURL           string `toml:"base_url"`
	AllowRegistration bool   `toml:"allow_registration"`

	// Warnings holds non-fatal problems found while loading, such as a config
	// file readable by other users. Callers should log them.
	Warnings []string `toml:"-"`
}

// Load reads the config file. Unknown keys are an error, so a typo cannot
// silently disable a setting. Relative paths are resolved against the
// directory containing the config file, not the working directory.
//
// Username and password are optional: when set and the database has no users
// yet, they create the first admin account. After that, users are managed with
// `tinysonic user …`.
func Load(path string) (*Config, error) {
	c := &Config{
		Port:         4533,
		Host:         "0.0.0.0",
		DatabasePath: "smolsonic.db",
		CoversDir:    "covers",
	}
	md, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("config file %s: unknown key(s): %s", path, strings.Join(names, ", "))
	}

	if c.MusicDir == "" {
		return nil, errors.New("config: music_dir must not be empty")
	}
	if (c.Username == "") != (c.Password == "") {
		return nil, errors.New("config: set both username and password, or neither")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	base := filepath.Dir(abs)
	c.MusicDir = resolve(base, c.MusicDir)
	c.DatabasePath = resolve(base, c.DatabasePath)
	c.CoversDir = resolve(base, c.CoversDir)

	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"config file %s is readable by other users (mode %o); it holds a password, run: chmod 600 %s",
			path, info.Mode().Perm(), path))
	}
	return c, nil
}

// resolve makes p absolute relative to base. Absolute paths are unchanged.
func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
