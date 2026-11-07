package bpm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// Window is the length of audio analysed per track, taken from the middle,
// where intros and fade-outs do not distort the tempo.
const Window = 90.0

// FindFFmpeg resolves the configured ffmpeg path, or looks ffmpeg up on PATH
// when configured is empty.
func FindFFmpeg(configured string) (string, error) {
	name := configured
	if name == "" {
		name = "ffmpeg"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found (%s): install ffmpeg or set ffmpeg_path", name)
	}
	return p, nil
}

// Decode runs ffmpeg at the lowest CPU priority and returns up to Window
// seconds of mono float samples at SampleRate from the middle of the file.
func Decode(ctx context.Context, ffmpeg, path string, durationSec float64) ([]float32, error) {
	start := math.Max(0, durationSec/2-Window/2)
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(start, 'f', 2, 64),
		"-t", strconv.FormatFloat(Window, 'f', 0, 64),
		"-i", path,
		"-vn", "-ac", "1", "-ar", strconv.Itoa(SampleRate), "-f", "f32le", "pipe:1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	LowerPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	b := stdout.Bytes()
	if len(b) < 4 {
		return nil, errors.New("ffmpeg produced no audio")
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
