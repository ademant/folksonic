package config

import (
	"fmt"
	"path/filepath"
	"slices"
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
		if name == ToolFFmpeg && t.Command == "" {
			// ffmpeg falls back to ffmpeg_path; only the timeout is set here.
		} else if !filepath.IsAbs(t.Command) {
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
