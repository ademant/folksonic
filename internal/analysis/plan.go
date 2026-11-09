package analysis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/bpm"
	"github.com/tsirysndr/tinysonic/internal/config"
)

// Group is one tool and the features assigned to it. A tool is run once per
// track for all of its features.
type Group struct {
	Tool     string
	Features []string
	timeout  time.Duration
	// memLimit and maxDuration come from the tool's config; 0 means none.
	memLimit    int64
	maxDuration time.Duration
	adapter     adapter
	// command is the configured path; for builtin and ffmpeg it may be empty,
	// meaning ffmpeg_path (resolved when a job starts).
	command string
}

// Plan is the [analysis] config turned into tool groups.
type Plan struct {
	Groups []*Group
	// Auto: analyse pending features after scans (the [analysis] table is
	// present).
	Auto       bool
	ffmpegPath string

	mu       sync.Mutex
	versions map[string]string
}

// NewPlan builds the plan from the loaded config. The config has already
// been validated by config.Load.
func NewPlan(a config.Analysis, ffmpegPath string) *Plan {
	p := &Plan{Auto: a.Auto, ffmpegPath: ffmpegPath, versions: map[string]string{}}
	byTool := map[string]*Group{}
	for _, f := range config.AnalysisFeatures {
		tool := a.Assignments()[f]
		if tool == "" {
			continue
		}
		g := byTool[tool]
		if g == nil {
			g = &Group{Tool: tool, timeout: config.DefaultToolTimeout}
			if t, ok := a.Tools[tool]; ok {
				g.command, g.timeout = t.Command, t.TimeoutDur
				g.memLimit, g.maxDuration = t.MemoryLimitBytes, t.MaxDurationDur
			}
			switch tool {
			case config.ToolBuiltin:
				g.adapter = builtinAdapter{}
			case config.ToolFFmpeg:
				g.adapter = ffmpegAdapter{}
			case "folksentia":
				g.adapter = folksentiaAdapter{}
			}
			byTool[tool] = g
			p.Groups = append(p.Groups, g)
		}
		g.Features = append(g.Features, f)
	}
	return p
}

// Tool returns the tool assigned to feature, or "".
func (p *Plan) Tool(feature string) string {
	for _, g := range p.Groups {
		if slices.Contains(g.Features, feature) {
			return g.Tool
		}
	}
	return ""
}

// Assignments returns feature → tool.
func (p *Plan) Assignments() map[string]string {
	m := map[string]string{}
	for _, g := range p.Groups {
		for _, f := range g.Features {
			m[f] = g.Tool
		}
	}
	return m
}

// ErrUnavailable is returned when a tool a job needs cannot be run.
var ErrUnavailable = errors.New("analysis tool not available")

// resolve returns the command to run for g: the configured path, or ffmpeg
// for the built-in estimator and ffmpeg without a configured command.
func (p *Plan) resolve(g *Group) (string, error) {
	if g.command != "" {
		info, err := os.Stat(g.command)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%w: %s: %s is not an executable file", ErrUnavailable, g.Tool, g.command)
		}
		return g.command, nil
	}
	ff, err := bpm.FindFFmpeg(p.ffmpegPath)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrUnavailable, g.Tool, err)
	}
	return ff, nil
}

// Available reports, per tool, why it cannot run ("" when it can).
func (p *Plan) Available() map[string]string {
	m := map[string]string{}
	for _, g := range p.Groups {
		if _, err := p.resolve(g); err != nil {
			m[g.Tool] = strings.TrimPrefix(err.Error(), ErrUnavailable.Error()+": ")
		} else {
			m[g.Tool] = ""
		}
	}
	return m
}

func (p *Plan) version(ctx context.Context, g *Group, command string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.versions[g.Tool]; ok {
		return v
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v := g.adapter.version(vctx, command)
	p.versions[g.Tool] = v
	return v
}
