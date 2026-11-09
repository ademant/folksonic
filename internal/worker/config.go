// Package worker is the desktop side of analysis workers (#78): it asks a
// tinysonic server for tracks, runs folksentia on them locally and sends the
// results back. It needs only the music directory and the tool; the server
// decides which features each track gets.
package worker

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tsirysndr/tinysonic/internal/config"
)

// Config is the worker's TOML file. It holds the worker key, so it is written
// with mode 0600.
type Config struct {
	// Server is the server's public address, including base_path, e.g.
	// https://cloud.example.org/folksonic. Plain http is accepted only for
	// localhost, because the key would travel unencrypted.
	Server string `toml:"server"`
	// APIKey is the worker key; `worker key` or `worker run --pair` writes it.
	APIKey string `toml:"api_key"`
	// MusicRoot is where the music directory is mounted on this machine. The
	// server's paths are relative to its own music directory.
	MusicRoot string `toml:"music_root"`
	// Batch is how many tracks one request asks for (default 5, at most 20).
	Batch int `toml:"batch"`
	// Folksentia runs the tool.
	Folksentia Tool `toml:"folksentia"`

	// Timeout and MemoryLimit are parsed by Load.
	timeout     time.Duration
	memoryLimit int64
}

// Tool is the [folksentia] table.
type Tool struct {
	// Command is an absolute path to folksentia on this machine.
	Command string `toml:"command"`
	// Timeout is a Go duration per track, default "2m".
	Timeout string `toml:"timeout"`
	// MemoryLimit caps the tool's memory, as in the server's config ("3G").
	MemoryLimit string `toml:"memory_limit"`
}

// Load reads and checks the worker file.
func Load(path string) (*Config, error) {
	c, err := Read(path)
	if err != nil {
		return nil, err
	}
	if err := c.Check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Read decodes the file without checking it, so a file that is still waiting
// for its key can be read when pairing.
func Read(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		return nil, fmt.Errorf("%s: unknown key %q", path, k)
	}
	return &c, nil
}

// Check reports what is wrong with the settings, and fills in the defaults.
func (c *Config) Check() error {
	u, err := url.Parse(c.Server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("server %q is not a URL", c.Server)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("server %q: use https; plain http is allowed only for localhost", c.Server)
		}
	default:
		return fmt.Errorf("server %q: scheme must be https", c.Server)
	}
	if c.APIKey == "" {
		return fmt.Errorf("api_key is empty; create the file with `tinysonic worker key` or redeem a pairing token with --pair")
	}
	if !filepath.IsAbs(c.MusicRoot) {
		return fmt.Errorf("music_root must be an absolute path, got %q", c.MusicRoot)
	}
	if c.Batch == 0 {
		c.Batch = 5
	}
	if c.Batch < 1 || c.Batch > 20 {
		return fmt.Errorf("batch must be between 1 and 20")
	}
	if !filepath.IsAbs(c.Folksentia.Command) {
		return fmt.Errorf("[folksentia] command must be an absolute path, got %q", c.Folksentia.Command)
	}
	info, err := os.Stat(c.Folksentia.Command)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("[folksentia] command %s is not an executable file", c.Folksentia.Command)
	}
	c.timeout = config.DefaultToolTimeout
	if c.Folksentia.Timeout != "" {
		d, err := time.ParseDuration(c.Folksentia.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("[folksentia] timeout %q is not a positive duration", c.Folksentia.Timeout)
		}
		c.timeout = d
	}
	c.memoryLimit = 0
	if c.Folksentia.MemoryLimit != "" {
		n, err := config.ParseByteSize(c.Folksentia.MemoryLimit)
		if err != nil {
			return fmt.Errorf("[folksentia] memory_limit %q: %v", c.Folksentia.MemoryLimit, err)
		}
		c.memoryLimit = n
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Save writes the file with mode 0600, replacing the old one. The comments of
// a hand-written file are not kept.
func Save(path string, c *Config) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
