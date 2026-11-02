package config

import (
	"errors"
	"fmt"

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
}

// Load reads the config file. Username and password are optional: when set
// and the database has no users yet, they create the first admin account.
// After that, users are managed with `tinysonic user …`.
func Load(path string) (*Config, error) {
	c := &Config{
		Port:         4533,
		Host:         "0.0.0.0",
		DatabasePath: "smolsonic.db",
		CoversDir:    "covers",
	}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	if c.MusicDir == "" {
		return nil, errors.New("config: music_dir must not be empty")
	}
	if (c.Username == "") != (c.Password == "") {
		return nil, errors.New("config: set both username and password, or neither")
	}
	return c, nil
}
