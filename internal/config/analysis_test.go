package config

import (
	"strings"
	"testing"
	"time"
)

func TestAnalysisDefaultIsManualBuiltinBPM(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Analysis.Auto || len(c.Analysis.Assignments()) != 1 || c.Analysis.BPM != ToolBuiltin {
		t.Fatalf("default analysis: %+v", c.Analysis)
	}
}

func TestAnalysisTable(t *testing.T) {
	c, err := Load(writeSiteConfig(t, `music_dirs = ["music"]
[analysis]
bpm      = "folksentia"
key      = "folksentia"
loudness = "ffmpeg"

[analysis.tools.folksentia]
command = "/usr/local/bin/folksentia"
timeout = "90s"
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Analysis
	m := a.Assignments()
	if !a.Auto || len(m) != 3 || m["bpm"] != "folksentia" || m["loudness"] != "ffmpeg" || m["beats"] != "" {
		t.Fatalf("assignments %v auto %v", m, a.Auto)
	}
	if tool := a.Tools["folksentia"]; tool.TimeoutDur != 90*time.Second || tool.Command != "/usr/local/bin/folksentia" {
		t.Fatalf("tool %+v", tool)
	}

	// An empty table switches automatic analysis on and assigns nothing.
	c, err = Load(writeSiteConfig(t, "music_dirs = [\"music\"]\n[analysis]\n"))
	if err != nil || !c.Analysis.Auto || len(c.Analysis.Assignments()) != 0 {
		t.Fatalf("empty table: %+v %v", c.Analysis, err)
	}
	c, err = Load(writeSiteConfig(t, "music_dirs = [\"music\"]\n[analysis]\nloudness = \"ffmpeg\"\n[analysis.tools.ffmpeg]\ntimeout = \"10m\"\n"))
	if err != nil || c.Analysis.Tools["ffmpeg"].TimeoutDur != 10*time.Minute {
		t.Fatalf("ffmpeg timeout only: %+v %v", c.Analysis, err)
	}
}

func TestAnalysisRejected(t *testing.T) {
	for cfg, want := range map[string]string{
		"[analysis]\nkey = \"builtin\"":     "builtin does not provide key",
		"[analysis]\nloudness = \"fpcalc\"": "unknown tool",
		"[analysis]\nkey = \"folksentia\"":  "[analysis.tools.folksentia] is not configured",
		"[analysis]\nfingerprint = \"folksentia\"\n[analysis.tools.folksentia]\ncommand = \"/x\"": "does not provide fingerprint",
		"[analysis]\nkey = \"ffmpeg\"":                                      "ffmpeg does not provide key",
		"[analysis.tools.folksentia]\ncommand = \"folksentia\"":             "absolute path",
		"[analysis.tools.folksentia]\ncommand = \"/x\"\ntimeout = \"soon\"": "positive duration",
		"[analysis.tools.essentia]\ncommand = \"/x\"":                       "unknown tool",
		"[analysis]\ntempo = \"builtin\"":                                   "unknown key",
	} {
		if _, err := Load(writeSiteConfig(t, "music_dirs = [\"music\"]\n"+cfg)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", cfg, err, want)
		}
	}
}
