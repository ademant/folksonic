package analysis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tsirysndr/tinysonic/internal/bpm"
)

// adapter runs one kind of tool. It maps the requested features to the
// tool's arguments and reads its output.
type adapter interface {
	// version identifies the tool's output, recorded with each value.
	version(ctx context.Context, command string) string
	run(ctx context.Context, command, path string, durationSec float64, features []string) (Values, error)
}

// ── builtin: pure-Go BPM estimate on audio decoded by ffmpeg ───────────────

type builtinAdapter struct{}

func (builtinAdapter) version(context.Context, string) string { return "1" }

func (builtinAdapter) run(ctx context.Context, ffmpeg, path string, durationSec float64, _ []string) (Values, error) {
	samples, err := bpm.Decode(ctx, ffmpeg, path, durationSec)
	if err != nil {
		return nil, err
	}
	tempo, conf, err := bpm.Estimate(samples)
	if err != nil {
		return nil, err
	}
	return Values{"bpm": &BPM{Value: tempo, Confidence: ptr(conf)}}, nil
}

// ── ffmpeg: EBU R128 loudness and true peak (ebur128 filter) ───────────────

type ffmpegAdapter struct{}

func (ffmpegAdapter) version(ctx context.Context, ffmpeg string) string {
	out, _, err := runTool(ctx, ffmpeg, []string{"-hide_banner", "-version"}, nil)
	if err != nil {
		return ""
	}
	// "ffmpeg version 7.1.5-0+deb13u1 Copyright …"
	f := strings.Fields(string(out))
	if len(f) >= 3 && f[1] == "version" {
		return f[2]
	}
	return ""
}

func (ffmpegAdapter) run(ctx context.Context, ffmpeg, path string, _ float64, _ []string) (Values, error) {
	_, stderr, err := runTool(ctx, ffmpeg, []string{
		"-nostdin", "-hide_banner", "-nostats", "-loglevel", "info",
		"-i", path, "-vn", "-af", "ebur128=peak=true:framelog=verbose", "-f", "null", "-",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	l, err := parseEBUR128(stderr)
	if err != nil {
		return nil, err
	}
	return Values{"loudness": l}, nil
}

// parseEBUR128 reads the summary the ebur128 filter prints at the end:
// "I: -9.4 LUFS" under Integrated loudness and "Peak: -0.8 dBFS" under
// True peak. -70 LUFS (the gate floor) and -inf mean silence: unknown.
func parseEBUR128(stderr []byte) (*Loudness, error) {
	i := bytes.LastIndex(stderr, []byte("Summary:"))
	if i < 0 {
		return nil, errors.New("ffmpeg: no ebur128 summary in output")
	}
	var l Loudness
	found := false
	sc := bufio.NewScanner(bytes.NewReader(stderr[i:]))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		valid := err == nil && !math.IsInf(v, 0) && !math.IsNaN(v)
		switch f[0] {
		case "I:":
			found = true
			if valid && v > -70 {
				l.IntegratedLufs = ptr(v)
			}
		case "Peak:":
			if valid {
				l.TruePeakDbtp = ptr(v)
			}
		}
	}
	if !found {
		return nil, errors.New("ffmpeg: no integrated loudness in ebur128 summary")
	}
	return &l, nil
}

// ── folksentia: Essentia-based extractor, JSON output schema 1 ─────────────

// folksentiaBins is the waveform resolution requested from folksentia.
const folksentiaBins = 150

// essentiaMaxBeatConfidence is the top of RhythmExtractor2013's multifeature
// confidence scale.
const essentiaMaxBeatConfidence = 5.32

type folksentiaAdapter struct{}

// folksentia has no --version; the output schema it is read with is what
// the values depend on.
func (folksentiaAdapter) version(context.Context, string) string { return "schema-1" }

func folksentiaArgs(features []string, path string) []string {
	var args []string
	if slices.Contains(features, "beats") {
		args = append(args, "--downbeats") // implies --beats
	}
	if slices.Contains(features, "waveform") {
		args = append(args, "--waveform="+strconv.Itoa(folksentiaBins))
	}
	if slices.Contains(features, "fingerprint") {
		args = append(args, "--fingerprint")
	}
	// "--" so a file name starting with "-" is not read as an option.
	return append(args, "--", path)
}

func (folksentiaAdapter) run(ctx context.Context, command, path string, _ float64, features []string) (Values, error) {
	// Exit status 1 means "a file failed"; the error object says why.
	out, _, err := runTool(ctx, command, folksentiaArgs(features, path), func(code int) bool { return code == 1 })
	if err != nil {
		return nil, fmt.Errorf("folksentia: %w", err)
	}
	return parseFolksentia(out, features)
}

// folksentiaOutput is the part of folksentia's schema 1 that is used.
type folksentiaOutput struct {
	Schema        int      `json:"schema"`
	Error         *string  `json:"error"`
	BPM           *float64 `json:"bpm"`
	BPMConfidence *float64 `json:"bpm_confidence"`
	Key           *string  `json:"key"`
	Scale         *string  `json:"scale"`
	KeyStrength   *float64 `json:"key_strength"`
	Silence       *struct {
		LeadMs *float64 `json:"leadMs"`
		TailMs *float64 `json:"tailMs"`
	} `json:"silence"`
	Loudness *struct {
		IntegratedLufs *float64 `json:"integratedLufs"`
		TruePeakDbtp   *float64 `json:"truePeakDbtp"`
	} `json:"loudness"`
	Waveform *struct {
		Bins  int   `json:"bins"`
		Peaks []int `json:"peaks"`
	} `json:"waveform"`
	Beats           []float64    `json:"beats"`
	DownbeatIndexes []int        `json:"downbeatIndexes"`
	Fingerprint     *Fingerprint `json:"fingerprint"`
}

func parseFolksentia(out []byte, features []string) (Values, error) {
	var o folksentiaOutput
	if err := json.Unmarshal(out, &o); err != nil {
		return nil, fmt.Errorf("folksentia: reading output: %w", err)
	}
	if o.Schema != 1 {
		return nil, fmt.Errorf("folksentia: output schema %d, expected 1", o.Schema)
	}
	if o.Error != nil {
		return nil, fmt.Errorf("folksentia: %s", shortMessage(*o.Error))
	}
	v := Values{}
	for _, f := range features {
		switch f {
		case "bpm":
			if o.BPM != nil && *o.BPM > 0 {
				b := &BPM{Value: *o.BPM}
				// Essentia's multifeature tracker rates its beats 0..5.32 (above
				// 3.5 is very reliable); it is scaled to 0..1 like the built-in
				// estimator. --fast is never passed, so 0 is a real value.
				if o.BPMConfidence != nil {
					b.Confidence = ptr(clamp01(*o.BPMConfidence / essentiaMaxBeatConfidence))
				}
				v[f] = b
			}
		case "key":
			if o.Key != nil && o.Scale != nil {
				if k, ok := ParseKey(*o.Key + " " + *o.Scale); ok {
					if o.KeyStrength != nil {
						k.Confidence = ptr(clamp01(*o.KeyStrength))
					}
					v[f] = k
				}
			}
		case "loudness":
			if o.Loudness != nil && (o.Loudness.IntegratedLufs != nil || o.Loudness.TruePeakDbtp != nil) {
				v[f] = &Loudness{IntegratedLufs: o.Loudness.IntegratedLufs, TruePeakDbtp: o.Loudness.TruePeakDbtp}
			}
		case "silence":
			if o.Silence != nil && (o.Silence.LeadMs != nil || o.Silence.TailMs != nil) {
				v[f] = &Silence{LeadMs: roundMs(o.Silence.LeadMs), TailMs: roundMs(o.Silence.TailMs)}
			}
		case "waveform":
			if o.Waveform != nil && o.Waveform.Bins > 0 && len(o.Waveform.Peaks) == o.Waveform.Bins {
				for _, p := range o.Waveform.Peaks {
					if p < 0 || p > 255 {
						return nil, fmt.Errorf("folksentia: waveform peak %d out of range", p)
					}
				}
				v[f] = &Waveform{Bins: o.Waveform.Bins, Peaks: o.Waveform.Peaks}
			}
		case "fingerprint":
			if fp := o.Fingerprint; fp != nil && fp.Value != "" {
				if fp.Algorithm != "chromaprint" {
					return nil, fmt.Errorf("folksentia: unknown fingerprint algorithm %q", fp.Algorithm)
				}
				if _, _, err := DecodeFingerprint(fp.Value); err != nil {
					return nil, fmt.Errorf("folksentia: %w", err)
				}
				v[f] = fp
			}
		case "beats":
			if o.Beats != nil {
				b := &Beats{TimesMs: make([]int64, len(o.Beats))}
				for i, s := range o.Beats {
					b.TimesMs[i] = int64(math.Round(s * 1000))
				}
				for _, i := range o.DownbeatIndexes {
					if i < 0 || i >= len(o.Beats) {
						return nil, fmt.Errorf("folksentia: downbeat index %d out of range", i)
					}
				}
				b.DownbeatIndexes = o.DownbeatIndexes
				v[f] = b
			}
		}
	}
	return v, nil
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

func roundMs(f *float64) *int64 {
	if f == nil {
		return nil
	}
	return ptr(int64(math.Round(*f)))
}

// ── key names, shared with the scanner's key tags ──────────────────────────

var tonics = map[string]string{
	"C": "C", "C#": "C#", "DB": "Db", "D": "D", "D#": "D#", "EB": "Eb", "E": "E",
	"F": "F", "F#": "F#", "GB": "Gb", "G": "G", "G#": "G#", "AB": "Ab", "A": "A",
	"A#": "A#", "BB": "Bb", "B": "B",
}

// ParseKey reads a key such as "A minor", "Am", "F#m", "Bb", "Bb major" or
// "C# min". Unknown notations (Camelot "8A", Open Key "1m") are rejected
// rather than guessed.
func ParseKey(s string) (*Key, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "♯", "#"), "♭", "b"))
	if s == "" {
		return nil, false
	}
	n := 1
	if len(s) > 1 && (s[1] == '#' || s[1] == 'b') {
		n = 2
	}
	tonic, ok := tonics[strings.ToUpper(s[:n])]
	if !ok {
		return nil, false
	}
	mode := ""
	switch strings.ToLower(strings.TrimSpace(s[n:])) {
	case "", "maj", "major", "dur":
		mode = "major"
	case "m", "min", "minor", "moll":
		mode = "minor"
	default:
		return nil, false
	}
	return &Key{Tonic: tonic, Mode: mode}, true
}
