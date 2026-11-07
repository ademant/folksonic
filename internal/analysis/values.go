// Package analysis computes per-track audio features (tempo, beats, key,
// loudness, silence, waveform) with the tools assigned in the [analysis]
// config table, and stores them in the analysis and song_values tables.
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

// Values is what one tool run returned, by feature. A missing or nil entry
// means the tool could not tell; the stored value is then left as it was.
type Values map[string]any

func ptr[T any](v T) *T { return &v }
