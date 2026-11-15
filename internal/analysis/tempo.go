package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
)

// The tempo of a song lives in the database: songs.bpm with its source and
// confidence, and the rhythm analysis in song_values. A file's BPM tag gives
// the first value and is never written back; a tool assigned to bpm replaces
// the stored value.
//
// Song objects carry the rounded bpm and, as the vendor field
// tinysonicTempo, the few values a player needs to judge it and change the
// playback speed. /api/analysis returns everything that is stored.

// TempoValues is what the tinysonicTempo keys are read from.
type TempoValues struct {
	Confidence *float64 // songs.bpm_confidence
	Rhythm     *Rhythm  // song_values 'rhythm', nil when not analysed
}

// TempoKey is one key of tinysonicTempo. Value returns nil when unknown.
type TempoKey struct {
	Name  string
	Value func(TempoValues) any
}

// TempoKeys are the keys of tinysonicTempo, in output order. Clients ignore
// keys they do not know; adding one bumps the tinysonicTempo extension
// version. A key read from another song_values row also needs that name in
// db.SongObjectValues, so a change reaches the change feed, and in the
// server's song query.
var TempoKeys = []TempoKey{
	{"confidence", func(t TempoValues) any { return deref(t.Confidence) }},
	{"barsPerMin", func(t TempoValues) any {
		if t.Rhythm == nil {
			return nil
		}
		return deref(t.Rhythm.BarsPerMin)
	}},
	{"barPulses", func(t TempoValues) any {
		if t.Rhythm == nil {
			return nil
		}
		return deref(t.Rhythm.BarPulses)
	}},
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// ParseTempo builds TempoValues from the stored columns. A rhythm value
// that does not parse counts as unknown.
func ParseTempo(confidence sql.NullFloat64, rhythm sql.NullString) TempoValues {
	var t TempoValues
	if confidence.Valid {
		t.Confidence = &confidence.Float64
	}
	if rhythm.Valid {
		var r Rhythm
		if json.Unmarshal([]byte(rhythm.String), &r) == nil {
			t.Rhythm = &r
		}
	}
	return t
}

// Object is tinysonicTempo: the known keys, or nil when none is known.
func (t TempoValues) Object() map[string]any {
	var out map[string]any
	for _, k := range TempoKeys {
		if v := k.Value(t); v != nil {
			if out == nil {
				out = map[string]any{}
			}
			out[k.Name] = v
		}
	}
	return out
}

// PulsePerMin is the measured pulse rate (60 / pulseSec), or 0 when unknown.
func (t TempoValues) PulsePerMin() float64 {
	if t.Rhythm == nil || t.Rhythm.PulseSec == nil || *t.Rhythm.PulseSec <= 0 {
		return 0
	}
	return 60 / *t.Rhythm.PulseSec
}

// tempoRatios are the relations of BPM to the pulse rate the report counts:
// the same tempo, double or half time, and the 3:2 cross-rhythms of folk
// dances.
var tempoRatios = []struct {
	Name  string
	Value float64
}{{"1", 1}, {"2", 2}, {"1/2", 0.5}, {"3/2", 1.5}, {"2/3", 2.0 / 3}}

// TempoRatioClasses are the names TempoRatio returns, in report order.
var TempoRatioClasses = func() []string {
	out := make([]string, 0, len(tempoRatios)+2)
	for _, r := range tempoRatios {
		out = append(out, r.Name)
	}
	return append(out, RatioOther, RatioUnknown)
}()

const (
	// RatioOther is a BPM that matches none of tempoRatios.
	RatioOther = "other"
	// RatioUnknown is a song without a BPM or without a measured pulse.
	RatioUnknown = "-"
)

// ratioTolerance is how far BPM / pulse rate may be from a ratio, relative
// to it: 4%, the margin the README gives for estimates that match the tag.
const ratioTolerance = 0.04

// TempoRatio classifies BPM against the pulse rate: one of tempoRatios'
// names, RatioOther, or RatioUnknown when either is missing.
func TempoRatio(bpm, pulsePerMin float64) string {
	if bpm <= 0 || pulsePerMin <= 0 {
		return RatioUnknown
	}
	r := bpm / pulsePerMin
	for _, c := range tempoRatios {
		if math.Abs(r/c.Value-1) <= ratioTolerance {
			return c.Name
		}
	}
	return RatioOther
}

// TempoRow is one song of the tempo report.
type TempoRow struct {
	ID, Artist, Title string
	BPM               sql.NullFloat64
	Source            sql.NullString // "tag", "calc" or the tool's name
	Tempo             TempoValues
	Ratio             string // TempoRatio of BPM and the pulse rate
}

// TempoReport reads the tempo of every song, ordered by artist and title.
// It only reads the database.
func TempoReport(ctx context.Context, db *sql.DB) ([]TempoRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, s.artist, s.title, s.bpm, s.bpm_source, s.bpm_confidence, sv.value
		FROM songs s
		LEFT JOIN song_values sv ON sv.song_id = s.id AND sv.name = 'rhythm'
		ORDER BY s.artist COLLATE NOCASE, s.title COLLATE NOCASE, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TempoRow
	for rows.Next() {
		var r TempoRow
		var conf sql.NullFloat64
		var rhythm sql.NullString
		if err := rows.Scan(&r.ID, &r.Artist, &r.Title, &r.BPM, &r.Source, &conf, &rhythm); err != nil {
			return nil, err
		}
		r.Tempo = ParseTempo(conf, rhythm)
		r.Ratio = TempoRatio(r.BPM.Float64, r.Tempo.PulsePerMin())
		out = append(out, r)
	}
	return out, rows.Err()
}
