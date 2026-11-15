package server

import (
	"github.com/tsirysndr/tinysonic/internal/analysis"
	"github.com/tsirysndr/tinysonic/internal/models"
)

// songTempo is the vendor field tinysonicTempo of a song: the stored values
// a player needs to judge its bpm and change the playback speed, or nil when
// none is known. The keys are analysis.TempoKeys; /api/analysis has the rest.
func songTempo(s models.Song) J {
	if obj := analysis.ParseTempo(s.BPMConfidence, s.Rhythm).Object(); obj != nil {
		return J(obj)
	}
	return nil
}
