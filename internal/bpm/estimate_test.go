package bpm

import (
	"math"
	"math/rand"
	"testing"
)

// clickTrack renders seconds of short decaying 1 kHz clicks at bpm, with
// every accentEvery-th click louder (0 = no accents) and a little noise.
func clickTrack(bpm float64, seconds float64, accentEvery int) []float32 {
	n := int(seconds * SampleRate)
	out := make([]float32, n)
	rng := rand.New(rand.NewSource(1))
	for i := range out {
		out[i] = float32(rng.NormFloat64() * 0.002)
	}
	period := 60 / bpm * SampleRate
	for beat := 0; ; beat++ {
		start := int(float64(beat) * period)
		if start >= n {
			break
		}
		amp := 0.5
		if accentEvery > 0 && beat%accentEvery == 0 {
			amp = 1
		}
		for i := 0; i < 600 && start+i < n; i++ {
			t := float64(i) / SampleRate
			out[start+i] += float32(amp * math.Exp(-t*60) * math.Sin(2*math.Pi*1000*t))
		}
	}
	return out
}

func TestEstimateClickTracks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bpm    float64
		accent int
	}{
		{"slow", 72, 0},
		{"march 2/4", 120, 2},
		{"reel", 110, 4},
		{"fast", 150, 0},
		{"waltz 3/4", 165, 3},
		{"mazurka 3/4", 130, 3},
		{"andro 4/4", 96, 4},
	} {
		got, conf, err := Estimate(clickTrack(tc.bpm, 60, tc.accent))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if math.Abs(got-tc.bpm)/tc.bpm > 0.02 {
			t.Errorf("%s: estimated %.1f BPM, want %.0f (±2%%)", tc.name, got, tc.bpm)
		}
		if conf <= 0.2 || conf > 1 {
			t.Errorf("%s: confidence %.2f", tc.name, conf)
		}
	}
}

func TestEstimateRejectsSilenceAndShortInput(t *testing.T) {
	if _, _, err := Estimate(make([]float32, SampleRate)); err != ErrTooShort {
		t.Errorf("1 s: err = %v, want ErrTooShort", err)
	}
	if _, _, err := Estimate(make([]float32, 30*SampleRate)); err != ErrNoPulse {
		t.Errorf("silence: err = %v, want ErrNoPulse", err)
	}
}

func TestFFT(t *testing.T) {
	a := make([]complex128, 8)
	for i := range a {
		a[i] = complex(math.Cos(2*math.Pi*float64(i)/8), 0)
	}
	fft(a)
	if math.Abs(real(a[1])-4) > 1e-9 || math.Abs(real(a[7])-4) > 1e-9 || math.Abs(real(a[0])) > 1e-9 {
		t.Fatalf("fft of cos: %v", a)
	}
}
