// Package bpm estimates the tempo of a recording. Decoding is done by an
// external ffmpeg (see Decode); the estimation itself is pure Go.
package bpm

import (
	"errors"
	"math"
	"math/cmplx"
)

const (
	// SampleRate is the mono rate Decode produces and Estimate expects.
	SampleRate = 11025
	frameSize  = 1024
	hopSize    = 256
	minBPM     = 40
	maxBPM     = 240
)

// priorCenter and priorWidth (in octaves) weight tempi like a listener
// would: values far from the centre need clearly stronger evidence.
// harmonics is how many multiples of a period the score includes. The values
// were chosen on 80 tagged tracks of a folk dance library: 61% came within 4%
// of their BPM tag (46% without harmonics and with a 1-octave prior); most of
// the rest are 3:4 cross-rhythms, which no setting resolved.
const (
	priorCenter = 120.0
	priorWidth  = 0.5
	harmonics   = 2
)

// ErrTooShort is returned when there is too little audio to estimate from.
var ErrTooShort = errors.New("bpm: not enough audio")

// ErrNoPulse is returned for silence or audio without a periodic pulse.
var ErrNoPulse = errors.New("bpm: no periodic pulse found")

// Estimate returns the tempo in beats per minute of mono samples at
// SampleRate, and a confidence between 0 and 1 (the normalised
// autocorrelation of the onset envelope at the chosen beat period).
//
// Method: spectral-flux onset envelope (log-magnitude increases summed over
// frequency, local mean removed, half-wave rectified), autocorrelation over
// the lags for 40–240 BPM scored by harmonic summation (the period and its
// multiples) and weighted by a log-normal tempo prior; the peak is refined by
// parabolic interpolation.
func Estimate(samples []float32) (bpm, confidence float64, err error) {
	env := onsetEnvelope(samples)
	fps := float64(SampleRate) / hopSize
	minLag := int(math.Floor(60 * fps / maxBPM))
	maxLag := int(math.Ceil(60 * fps / minBPM))
	if len(env) < 2*maxLag+1 {
		return 0, 0, ErrTooShort
	}

	// Harmonic summation looks at multiples of each candidate period, so the
	// autocorrelation is needed up to harmonics*maxLag.
	if len(env) < harmonics*maxLag+2 {
		return 0, 0, ErrTooShort
	}
	ac := make([]float64, harmonics*maxLag+2)
	for lag := range ac {
		var sum float64
		for i := lag; i < len(env); i++ {
			sum += env[i] * env[i-lag]
		}
		ac[lag] = sum / float64(len(env)-lag)
	}
	if ac[0] <= 0 {
		return 0, 0, ErrNoPulse
	}

	best, bestScore := -1, 0.0
	for lag := minLag; lag <= maxLag; lag++ {
		if ac[lag] <= ac[lag-1] || ac[lag] < ac[lag+1] {
			continue // only local maxima
		}
		t := 60 * fps / float64(lag)
		w := math.Exp(-0.5 * math.Pow(math.Log2(t/priorCenter)/priorWidth, 2))
		// A true beat period repeats at its multiples (bars, phrases); a
		// cross-rhythm such as 3 against 4 does not. Sum the harmonics,
		// each at its best value within one frame.
		var sum float64
		for h := 1; h <= harmonics; h++ {
			sum += peakNear(ac, h*lag, h) / float64(h)
		}
		if score := sum * w; score > bestScore {
			best, bestScore = lag, score
		}
	}
	if best < 0 {
		return 0, 0, ErrNoPulse
	}
	// Parabolic interpolation around the peak for sub-frame precision.
	lag := float64(best)
	if a, b, c := ac[best-1], ac[best], ac[best+1]; a-2*b+c != 0 {
		lag += 0.5 * (a - c) / (a - 2*b + c)
	}
	conf := math.Max(0, math.Min(1, ac[best]/ac[0]))
	return math.Round(60*fps/lag*10) / 10, conf, nil
}

// peakNear is the largest ac value within ±tol of i.
func peakNear(ac []float64, i, tol int) float64 {
	best := 0.0
	for j := max(0, i-tol); j <= min(len(ac)-1, i+tol); j++ {
		best = max(best, ac[j])
	}
	return best
}

// onsetEnvelope is the spectral flux per hop, with the local mean (about one
// second) subtracted and negative values clipped.
func onsetEnvelope(samples []float32) []float64 {
	if len(samples) < frameSize {
		return nil
	}
	n := (len(samples)-frameSize)/hopSize + 1
	window := make([]float64, frameSize)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/frameSize)
	}
	buf := make([]complex128, frameSize)
	prev := make([]float64, frameSize/2)
	cur := make([]float64, frameSize/2)
	flux := make([]float64, n)
	for f := 0; f < n; f++ {
		off := f * hopSize
		for i := range buf {
			buf[i] = complex(float64(samples[off+i])*window[i], 0)
		}
		fft(buf)
		var sum float64
		for k := range cur {
			cur[k] = math.Log1p(1000 * cmplx.Abs(buf[k]))
			if f > 0 {
				if d := cur[k] - prev[k]; d > 0 {
					sum += d
				}
			}
		}
		flux[f] = sum
		prev, cur = cur, prev
	}
	// Remove the local mean so sustained loudness changes do not count.
	const half = 22 // ~0.5 s each side at 43 frames/s
	env := make([]float64, n)
	var acc float64
	for i := 0; i < n; i++ {
		lo, hi := max(0, i-half), min(n-1, i+half)
		if i == 0 {
			for j := lo; j <= hi; j++ {
				acc += flux[j]
			}
		} else {
			if j := i + half; j < n {
				acc += flux[j]
			}
			if j := i - half - 1; j >= 0 {
				acc -= flux[j]
			}
		}
		if v := flux[i] - acc/float64(hi-lo+1); v > 0 {
			env[i] = v
		}
	}
	return env
}

// fft is an in-place iterative radix-2 FFT; len(a) must be a power of two.
func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, v := a[start+k], a[start+k+size/2]*wk
				a[start+k], a[start+k+size/2] = u+v, u-v
				wk *= w
			}
		}
	}
}
