package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AnalysisFeatures are the per-track analysis features, in output order.
var AnalysisFeatures = []string{"bpm", "beats", "key", "loudness", "silence", "waveform", "fingerprint"}

// Built-in analysis tools, which need no [analysis.tools] entry. "builtin" is
// the pure-Go BPM estimator (decoding through ffmpeg_path); "ffmpeg" is
// ffmpeg_path itself.
const (
	ToolBuiltin = "builtin"
	ToolFFmpeg  = "ffmpeg"
)

// DefaultToolTimeout bounds one tool run on one track when timeout is unset.
const DefaultToolTimeout = 2 * time.Minute

// Analysis is the [analysis] table: which tool computes which feature. A
// feature without a tool is not computed. Without the table, bpm uses the
// built-in estimator and runs only when started from the library page, as
// before; with the table, assigned features are analysed in the background
// after every scan.
type Analysis struct {
	BPM         string `toml:"bpm"`
	Beats       string `toml:"beats"`
	Key         string `toml:"key"`
	Loudness    string `toml:"loudness"`
	Silence     string `toml:"silence"`
	Waveform    string `toml:"waveform"`
	Fingerprint string `toml:"fingerprint"`
	// Tools configures the external tools by name. The name selects the
	// adapter (how features map to arguments and how output is read), so only
	// known names are accepted.
	Tools map[string]AnalysisTool `toml:"tools"`

	// Auto is true when the [analysis] table is present: assigned features
	// are then analysed after scans without being started by hand.
	Auto bool `toml:"-"`
}

// AnalysisTool is one [analysis.tools.<name>] entry. The command is an
// absolute path; it is never looked up on PATH.
type AnalysisTool struct {
	Command string `toml:"command"`
	// Timeout is a Go duration such as "120s". Empty means DefaultToolTimeout.
	Timeout string `toml:"timeout"`
	// TimeoutDur is Timeout parsed by Load.
	TimeoutDur time.Duration `toml:"-"`
	// Worker was the switch for network analysis workers, removed in #105.
	// It is still read so that a config that sets it fails with a hint
	// instead of silently running the tool here.
	Worker bool `toml:"worker"`
	// MemoryLimit caps the memory the tool may allocate, as a size such as
	// "500M" (K, M or G, binary; plain bytes without a suffix). Empty means no
	// limit. It is applied with RLIMIT_DATA on Linux, so a tool that needs more
	// fails with an error for that track instead of pushing the host into the
	// OOM killer. Set after the tool starts, so a tool that allocates heavily
	// during its first moments is not limited.
	MemoryLimit string `toml:"memory_limit"`
	// MemoryLimitBytes is MemoryLimit parsed by Load; 0 means no limit.
	MemoryLimitBytes int64 `toml:"-"`
	// MaxDuration skips tracks longer than this for this tool, as a Go duration
	// such as "45m". The skipped track gets a stored reason. Empty means no
	// limit.
	MaxDuration string `toml:"max_duration"`
	// MaxDurationDur is MaxDuration parsed by Load; 0 means no limit.
	MaxDurationDur time.Duration `toml:"-"`
}

// ExternalAnalysisTools are the tool names an [analysis.tools] entry may use.
var ExternalAnalysisTools = []string{"folksentia", ToolFFmpeg}

// ToolFeatures lists the features each tool's adapter can provide.
var ToolFeatures = map[string][]string{
	ToolBuiltin:  {"bpm"},
	ToolFFmpeg:   {"loudness"},
	"folksentia": {"bpm", "beats", "key", "loudness", "silence", "waveform", "fingerprint"},
}

// Assignments returns feature → tool for every assigned feature.
func (a Analysis) Assignments() map[string]string {
	m := map[string]string{}
	for i, t := range []string{a.BPM, a.Beats, a.Key, a.Loudness, a.Silence, a.Waveform, a.Fingerprint} {
		if t != "" {
			m[AnalysisFeatures[i]] = t
		}
	}
	return m
}

func (a *Analysis) normalise(defined bool) error {
	a.Auto = defined
	if !defined {
		a.BPM = ToolBuiltin
	}
	for name, t := range a.Tools {
		if !slices.Contains(ExternalAnalysisTools, name) {
			return fmt.Errorf("config: analysis.tools.%s: unknown tool (known: %v)", name, ExternalAnalysisTools)
		}
		if t.Worker {
			return fmt.Errorf("config: analysis.tools.%s: worker = true is no longer supported; analysis workers were replaced by `tinysonic analysis export` and `import` (leave command unset to analyse %s offline only)", name, name)
		}
		switch {
		case t.Command == "" && (name == ToolFFmpeg || name == "folksentia"):
			// ffmpeg falls back to ffmpeg_path. folksentia without a command
			// is offline only: its results come in through `analysis import`.
		case !filepath.IsAbs(t.Command):
			return fmt.Errorf("config: analysis.tools.%s.command must be an absolute path, got %q", name, t.Command)
		}
		t.TimeoutDur = DefaultToolTimeout
		if t.Timeout != "" {
			d, err := time.ParseDuration(t.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("config: analysis.tools.%s.timeout %q is not a positive duration such as \"120s\"", name, t.Timeout)
			}
			t.TimeoutDur = d
		}
		if t.MemoryLimit != "" {
			n, err := ParseByteSize(t.MemoryLimit)
			if err != nil {
				return fmt.Errorf("config: analysis.tools.%s.memory_limit %q: %v (use a size such as \"500M\")", name, t.MemoryLimit, err)
			}
			t.MemoryLimitBytes = n
		}
		if t.MaxDuration != "" {
			d, err := time.ParseDuration(t.MaxDuration)
			if err != nil || d <= 0 {
				return fmt.Errorf("config: analysis.tools.%s.max_duration %q is not a positive duration such as \"45m\"", name, t.MaxDuration)
			}
			t.MaxDurationDur = d
		}
		a.Tools[name] = t
	}
	for feature, tool := range a.Assignments() {
		supported, known := ToolFeatures[tool]
		if !known {
			return fmt.Errorf("config: analysis.%s: unknown tool %q", feature, tool)
		}
		if !slices.Contains(supported, feature) {
			return fmt.Errorf("config: analysis.%s: %s does not provide %s (it provides %v)", feature, tool, feature, supported)
		}
		if _, ok := a.Tools[tool]; !ok && tool != ToolBuiltin && tool != ToolFFmpeg {
			return fmt.Errorf("config: analysis.%s = %q, but [analysis.tools.%s] is not configured", feature, tool, tool)
		}
	}
	return nil
}

// ParseByteSize reads a size such as "500M", "2G" or "1048576". K, M and G
// are binary (multiples of 1024).
func ParseByteSize(s string) (int64, error) {
	mult := int64(1)
	v := strings.TrimSpace(s)
	if n := len(v); n > 0 {
		switch v[n-1] {
		case 'k', 'K':
			mult, v = 1<<10, v[:n-1]
		case 'm', 'M':
			mult, v = 1<<20, v[:n-1]
		case 'g', 'G':
			mult, v = 1<<30, v[:n-1]
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive size")
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("too large")
	}
	return n * mult, nil
}
