// Package analysis computes per-track audio features (tempo, beats, key,
// loudness, silence, waveform, fingerprint, onsets, rhythm, spectral) with
// the tools assigned in the [analysis] config table, and stores them in the
// analysis and song_values tables.
// It runs only in background jobs, never while a request is served.
package analysis

// The value types below are stored as JSON in song_values and returned by
// /api/analysis as they are. Pointer fields are null when unknown.

// BPM is a tempo. It lives in the songs table (bpm, bpm_confidence), not in
// song_values, because Subsonic responses carry it.
type BPM struct {
	Value      float64  `json:"value"`
	Confidence *float64 `json:"confidence"`
}

// Beats are beat positions and the indexes of the beats that start a bar.
type Beats struct {
	TimesMs         []int64 `json:"timesMs"`
	DownbeatIndexes []int   `json:"downbeatIndexes"`
}

// Key is the musical key: tonic such as "A" or "F#", mode "major" or "minor".
type Key struct {
	Tonic      string   `json:"tonic"`
	Mode       string   `json:"mode"`
	Confidence *float64 `json:"confidence"`
}

// Loudness is the EBU R128 integrated loudness and the true peak.
type Loudness struct {
	IntegratedLufs *float64 `json:"integratedLufs"`
	TruePeakDbtp   *float64 `json:"truePeakDbtp"`
}

// Silence is the silence before the first and after the last sound.
type Silence struct {
	LeadMs *int64 `json:"leadMs"`
	TailMs *int64 `json:"tailMs"`
}

// Waveform holds peaks 0..255 per bin, normalised to the loudest bin.
type Waveform struct {
	Bins  int   `json:"bins"`
	Peaks []int `json:"peaks"`
}

// Onsets are note starts and their strength 0..255, relative to the
// track's strongest onset. Both slices have the same length; empty when no
// onset was found.
type Onsets struct {
	TimesMs   []int64 `json:"timesMs"`
	Strengths []int   `json:"strengths"`
}

// Rhythm is the music's own cycle: the pulse, the bar as a whole number of
// pulses, the bars per minute dancers count, and how clearly the bar
// recurs (0..1).
type Rhythm struct {
	PulseSec   *float64 `json:"pulseSec"`
	BarPulses  *int     `json:"barPulses"`
	BarsPerMin *float64 `json:"barsPerMin"`
	BarScore   *float64 `json:"barScore"`
}

// Spectral says how many frequencies the sound uses: complexity is the
// median number of spectral peaks per frame, entropy the median spectral
// entropy in bits (low for a few tones, high for noise or percussion).
type Spectral struct {
	Complexity *float64 `json:"complexity"`
	Entropy    *float64 `json:"entropy"`
}

// Values is what one tool run returned, by feature. A missing or nil entry
// means the tool could not tell; the stored value is then left as it was.
type Values map[string]any

func ptr[T any](v T) *T { return &v }
