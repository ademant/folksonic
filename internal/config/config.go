package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tsirysndr/tinysonic/internal/logging"
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
	// FFmpegPath is the ffmpeg binary used to decode audio for BPM
	// calculation. Empty means look ffmpeg up on PATH. Without ffmpeg, BPM
	// calculation is unavailable and everything else works.
	FFmpegPath string `toml:"ffmpeg_path"`
	// OIDC enables sign-in through an external identity provider. It is off
	// unless Issuer is set; see OIDC for the rules.
	OIDC OIDC `toml:"oidc"`
	// LogLevel is the minimum level written to the log: debug, info, warn or
	// error. Load normalises it to lower case; empty means info.
	LogLevel string `toml:"log_level"`
	// Site is shown on the public information page; see Site.
	Site Site `toml:"site"`
	// LegacyPasswordAuth allows Subsonic clients to sign in with u and p (plain
	// or enc:-hex) on every request. Off, those requests get error 42 and API
	// keys are the only way in. Load sets the default, true.
	LegacyPasswordAuth bool `toml:"legacy_password_auth"`
	// MaxStreams caps the audio streams served at once, across all accounts.
	// Zero disables the cap. Load sets the default.
	MaxStreams int `toml:"max_streams"`
	// MaxStreamsPerUser caps the streams one account may hold at once. Zero
	// disables the cap. Load sets the default.
	MaxStreamsPerUser int `toml:"max_streams_per_user"`
	// DBCacheMB is the SQLite page cache per connection, in MiB. Load sets the
	// default; values below 1 are refused.
	DBCacheMB int `toml:"db_cache_mb"`
	// TrustedProxies lists the addresses (IPs or CIDR ranges) of reverse
	// proxies. Only a request from one of them has its X-Forwarded-For header
	// believed when the client's address is logged and rate limited.
	TrustedProxies []string `toml:"trusted_proxies"`
	// APIKeyTTLDays is how long a new API key lives, in days, unless the
	// request gives its own expires_days. Every key expires. Load sets the
	// default (365).
	APIKeyTTLDays int `toml:"api_key_ttl_days"`
	// APIKeyIdleDays revokes a key that has not been used for this many days.
	// Load sets the default (180).
	APIKeyIdleDays int `toml:"api_key_idle_days"`
	// RequireHTTPS refuses plain HTTP on every endpoint. Unset, it is on when
	// base_url is a public name and off for a private or loopback address; see
	// HTTPSRequired.
	RequireHTTPS *bool `toml:"require_https"`
	// TLSCert and TLSKey serve HTTPS directly, without a reverse proxy: a PEM
	// certificate and its private key. Both or neither. `tinysonic cert` makes
	// them. Clients pin the certificate: the pairing QR carries its pin.
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// Analysis assigns the per-track analysis features to tools; see
	// Analysis.
	Analysis Analysis `toml:"analysis"`

	// Warnings holds non-fatal problems found while loading, such as a config
	// file readable by other users. Callers should log them.
	Warnings []string `toml:"-"`
}

// Defaults for the stream limits, used when the key is absent. Zero in the
// file disables the limit.
const (
	DefaultMaxStreams        = 64
	DefaultMaxStreamsPerUser = 8
	DefaultDBCacheMB         = 4
	// DefaultAPIKeyTTLDays and DefaultAPIKeyIdleDays are the key lifetimes
	// used when the file does not set them.
	DefaultAPIKeyTTLDays  = 365
	DefaultAPIKeyIdleDays = 180
)

// ParseTrustedProxy accepts one trusted_proxies entry: an IP address or a CIDR
// range.
func ParseTrustedProxy(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an IP address or CIDR range", s)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// DefaultRepository is the repository link shown when [site] repository is empty.
const DefaultRepository = "https://github.com/ademant/folksonic"

// Site is the public description of this server, shown on the information
// page at / and in robots.txt and sitemap.xml. Empty fields are not shown.
type Site struct {
	// Name is the server's display name. Empty means "tinysonic".
	Name string `toml:"name"`
	// Description is a short paragraph about the server.
	Description string `toml:"description"`
	// Contact is how to reach the operator: an email address or a URL.
	Contact string `toml:"contact"`
	// Repository links the project's source code, which also hosts the API
	// documentation. Load sets DefaultRepository when this is empty.
	Repository string `toml:"repository"`
	// ImpressumFile is a Markdown file with the legal notice, served at
	// /impressum. Relative paths resolve from the config file's directory.
	// Empty means there is no Impressum page.
	ImpressumFile string `toml:"impressum_file"`
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
		Port:           4533,
		Host:           "0.0.0.0",
		DatabasePath:   "smolsonic.db",
		CoversDir:      "covers",
		APIKeyTTLDays:  DefaultAPIKeyTTLDays,
		APIKeyIdleDays: DefaultAPIKeyIdleDays,
		DBCacheMB:      DefaultDBCacheMB,
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
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("config: tls_cert and tls_key must be set together")
	}
	if c.TLSCert != "" && !strings.HasPrefix(strings.ToLower(c.BaseURL), "https://") {
		c.Warnings = append(c.Warnings, "tls_cert is set but base_url is not https://; set base_url to the https address clients use")
	}
	lvl, err := logging.ParseLevel(c.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("config: log_level: %w", err)
	}
	c.LogLevel = strings.ToLower(lvl.String())
	if !md.IsDefined("legacy_password_auth") {
		c.LegacyPasswordAuth = true
	}
	if c.DBCacheMB < 1 {
		return nil, errors.New("config: db_cache_mb must be at least 1")
	}
	if c.MaxStreams < 0 || c.MaxStreamsPerUser < 0 {
		return nil, errors.New("config: max_streams and max_streams_per_user must not be negative")
	}
	if !md.IsDefined("max_streams") {
		c.MaxStreams = DefaultMaxStreams
	}
	if !md.IsDefined("max_streams_per_user") {
		c.MaxStreamsPerUser = DefaultMaxStreamsPerUser
	}
	for _, p := range c.TrustedProxies {
		if _, err := ParseTrustedProxy(p); err != nil {
			return nil, fmt.Errorf("config: trusted_proxies: %w", err)
		}
	}
	if c.APIKeyTTLDays < 1 || c.APIKeyTTLDays > 3650 {
		return nil, errors.New("config: api_key_ttl_days must be between 1 and 3650")
	}
	if c.APIKeyIdleDays < 1 || c.APIKeyIdleDays > 3650 {
		return nil, errors.New("config: api_key_idle_days must be between 1 and 3650")
	}
	if err := c.Analysis.normalise(md.IsDefined("analysis")); err != nil {
		return nil, err
	}
	if c.Site.Repository == "" {
		c.Site.Repository = DefaultRepository
	}
	if u, err := url.Parse(c.Site.Repository); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("config: site.repository %q must be an http or https URL", c.Site.Repository)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	base := filepath.Dir(abs)
	if c.Site.ImpressumFile != "" {
		c.Site.ImpressumFile = resolveAll(base, []string{c.Site.ImpressumFile})[0]
	}
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

	// Only a secret in the file makes loose permissions a problem.
	secret := c.Password != "" || c.OIDC.ClientSecret != ""
	if info, err := os.Stat(path); err == nil && secret && info.Mode().Perm()&0o077 != 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"config file %s is readable by other users (mode %o); it holds a secret, run: chmod 600 %s",
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

// HTTPSRequired reports whether plain HTTP is refused on every endpoint. It is
// on unless require_https says otherwise: a LAN install is served over HTTPS
// too, with tls_cert and tls_key (see tinysonic cert). Set require_https =
// false only for a test setup on a trusted network.
func (c *Config) HTTPSRequired() bool {
	if c.RequireHTTPS != nil {
		return *c.RequireHTTPS
	}
	return true
}
