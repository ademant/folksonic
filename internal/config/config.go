package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// MusicDirs are the library roots, scanned and watched recursively.
	MusicDirs []string `toml:"music_dirs"`
	// MusicDir is the older single-directory key. It is kept as an alias and
	// folded into MusicDirs by Load; setting both keys is an error.
	MusicDir     string `toml:"music_dir"`
	Username     string `toml:"username"`
	Password     string `toml:"password"`
	Port         int    `toml:"port"`
	Host         string `toml:"host"`
	DatabasePath string `toml:"database_path"`
	CoversDir    string `toml:"covers_dir"`
	BaseURL      string `toml:"base_url"`
	// BasePath serves tinysonic under a URL prefix, such as /folksonic. It is
	// normalised by Load to a leading slash with no trailing slash; "/" and ""
	// both mean the root.
	BasePath          string `toml:"base_path"`
	AllowRegistration bool   `toml:"allow_registration"`
	// SongTags lists the file tags exposed to clients (for example DANCE and
	// USE). Normalised by Load to upper case, without duplicates.
	SongTags []string `toml:"song_tags"`
	// IgnoredArticles are leading words skipped when indexing and sorting
	// artists, separated by spaces. An article ending in an apostrophe (L')
	// matches without a following space. Load sets the default.
	IgnoredArticles string `toml:"ignored_articles"`
	// OIDC enables sign-in through an external identity provider. It is off
	// unless Issuer is set; see OIDC for the rules.
	OIDC OIDC `toml:"oidc"`

	// Warnings holds non-fatal problems found while loading, such as a config
	// file readable by other users. Callers should log them.
	Warnings []string `toml:"-"`
}

// Load reads the config file. Unknown keys are an error, so a typo cannot
// silently disable a setting. Relative paths are resolved against the
// directory containing the config file, not the working directory.
//
// Username and password are optional: when set and the database has no users
// yet, they create the first librarian account. After that, users are managed with
// `tinysonic user …`.
func Load(path string) (*Config, error) {
	c := &Config{
		Port:         4533,
		Host:         "0.0.0.0",
		DatabasePath: "smolsonic.db",
		CoversDir:    "covers",
		// Same list as server.DefaultIgnoredArticles.
		IgnoredArticles: "The El La Las Le Les Los Il L' An A Die Das Ein",
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

	if c.MusicDir != "" && len(c.MusicDirs) > 0 {
		return nil, errors.New("config: set music_dirs or music_dir, not both")
	}
	if c.MusicDir != "" {
		c.MusicDirs = []string{c.MusicDir}
		c.MusicDir = ""
	}
	if len(c.MusicDirs) == 0 {
		return nil, errors.New("config: music_dirs must list at least one directory")
	}
	if (c.Username == "") != (c.Password == "") {
		return nil, errors.New("config: set both username and password, or neither")
	}
	if err := c.normaliseBasePath(); err != nil {
		return nil, err
	}
	if err := c.normaliseSongTags(); err != nil {
		return nil, err
	}
	if err := c.OIDC.validate(c.BaseURL); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	base := filepath.Dir(abs)
	c.MusicDirs = uniqueDirs(resolveAll(base, c.MusicDirs))
	for _, d := range c.MusicDirs {
		// A missing directory is not fatal: the rest of the library keeps
		// working, and its songs are not pruned while it is unavailable.
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			c.Warnings = append(c.Warnings, fmt.Sprintf("music directory %s does not exist or is not a directory", d))
		}
	}
	c.DatabasePath = resolve(base, c.DatabasePath)
	c.CoversDir = resolve(base, c.CoversDir)

	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"config file %s is readable by other users (mode %o); it holds a password, run: chmod 600 %s",
			path, info.Mode().Perm(), path))
	}
	return c, nil
}

// normaliseBasePath checks base_path and strips a trailing slash. It also warns
// when base_url does not end with the prefix, because QR codes and clients
// would then point at the wrong place.
func (c *Config) normaliseBasePath() error {
	p := strings.TrimRight(c.BasePath, "/")
	if p == "" {
		c.BasePath = ""
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("config: base_path %q must start with /", c.BasePath)
	}
	if strings.ContainsAny(p, "?# \t") {
		return fmt.Errorf("config: base_path %q must be a plain path", c.BasePath)
	}
	c.BasePath = p
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || strings.TrimRight(u.Path, "/") != p {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"base_url %q does not end with base_path %q; pairing and clients need the full public address", c.BaseURL, p))
		}
	}
	return nil
}

func resolveAll(base string, ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p == "" {
			continue
		}
		out = append(out, resolve(base, p))
	}
	return out
}

// uniqueDirs removes duplicates after cleaning, keeping the first occurrence.
func uniqueDirs(ds []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range ds {
		c := filepath.Clean(d)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// resolve makes p absolute relative to base. Absolute paths are unchanged.
func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// OIDC configures single sign-on with an OpenID Connect provider. Only the
// issuer is required beyond the client ID: the endpoints come from the
// provider's discovery document. base_url must be set, because it defines the
// redirect address registered with the provider.
type OIDC struct {
	Issuer       string `toml:"issuer"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
}

// Enabled reports whether OIDC sign-in is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

func (o OIDC) validate(baseURL string) error {
	if !o.Enabled() {
		if o.ClientID != "" || o.ClientSecret != "" {
			return errors.New("config: oidc needs issuer")
		}
		return nil
	}
	u, err := url.Parse(o.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("config: oidc issuer %q must be an absolute http or https URL", o.Issuer)
	}
	if u.Scheme == "http" && !strings.HasPrefix(u.Host, "localhost") && !strings.HasPrefix(u.Host, "127.0.0.1") {
		return fmt.Errorf("config: oidc issuer %q must use https", o.Issuer)
	}
	if o.ClientID == "" {
		return errors.New("config: oidc needs client_id")
	}
	if baseURL == "" {
		return errors.New("config: oidc needs base_url, which the identity provider redirects back to")
	}
	return nil
}

// maxSongTags bounds the allowlist; every song response carries these values.
const maxSongTags = 32

// normaliseSongTags upper-cases the tag names, rejects names that cannot be
// Vorbis or ID3 field names, and removes duplicates.
func (c *Config) normaliseSongTags() error {
	seen := map[string]bool{}
	var out []string
	for _, raw := range c.SongTags {
		name := strings.ToUpper(strings.TrimSpace(raw))
		if name == "" {
			return errors.New("config: song_tags must not contain empty names")
		}
		for _, r := range name {
			if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return fmt.Errorf("config: song_tags name %q may only contain letters, digits and _", raw)
			}
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) > maxSongTags {
		return fmt.Errorf("config: song_tags lists %d names; the limit is %d", len(out), maxSongTags)
	}
	c.SongTags = out
	return nil
}
