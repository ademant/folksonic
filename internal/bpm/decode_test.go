package bpm

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"testing"
)

// End to end with a real ffmpeg, when one is installed: a generated 100 BPM
// click track is decoded from the middle and estimated.
func TestDecodeWithFFmpeg(t *testing.T) {
	ffmpeg, err := FindFFmpeg("")
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(t.TempDir(), "click.flac")
	// 0.6 s period = 100 BPM; 120 s long, so the middle window is used.
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i",
		"aevalsrc='if(lt(mod(t,0.6),0.03),sin(2*PI*1000*t),0)':s=22050:d=120", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate test audio: %v %s", err, out)
	}
	samples, err := Decode(context.Background(), ffmpeg, path, 120)
	if err != nil {
		t.Fatal(err)
	}
	if got := float64(len(samples)) / SampleRate; math.Abs(got-Window) > 1 {
		t.Fatalf("decoded %.1f s, want %.0f", got, Window)
	}
	bpm, _, err := Estimate(samples)
	if err != nil || math.Abs(bpm-100) > 2 {
		t.Fatalf("estimated %.1f BPM (%v), want 100", bpm, err)
	}
}

func TestFindFFmpegMissing(t *testing.T) {
	if _, err := FindFFmpeg("/nonexistent/ffmpeg"); err == nil {
		t.Fatal("missing ffmpeg not reported")
	}
}
