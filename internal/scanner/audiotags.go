package scanner

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dhowden/tag"

	"github.com/tsirysndr/tinysonic/internal/analysis"
)

// audioTags are the loudness, meter and tempo tags of a file. Unknown
// values are NULL.
type audioTags struct {
	TrackGain, AlbumGain, TrackPeak, AlbumPeak, BaseGain sql.NullFloat64
	Meter                                                sql.NullString
	BPM                                                  sql.NullFloat64
	// Key is the KEY / INITIALKEY / TKEY tag, when it names a key ParseKey
	// understands.
	Key *analysis.Key
}

// Tag names, upper case. MP3 user frames (TXXX) and MP4 freeform atoms use
// the same names as Vorbis comments; TBPM and tmpo are the ID3 and MP4 tempo
// fields.
var (
	r128Track   = "R128_TRACK_GAIN"
	r128Album   = "R128_ALBUM_GAIN"
	rgTrackGain = "REPLAYGAIN_TRACK_GAIN"
	rgAlbumGain = "REPLAYGAIN_ALBUM_GAIN"
	rgTrackPeak = "REPLAYGAIN_TRACK_PEAK"
	rgAlbumPeak = "REPLAYGAIN_ALBUM_PEAK"
	meterTags   = []string{"METER", "TIME_SIGNATURE", "TIMESIGNATURE"}
	bpmTags     = []string{"BPM", "TBPM", "TMPO", "TEMPO"}
	keyTags     = []string{"INITIALKEY", "KEY", "TKEY"}
)

func audioTagAllow() map[string]bool {
	m := map[string]bool{r128Track: true, r128Album: true, rgTrackGain: true, rgAlbumGain: true, rgTrackPeak: true, rgAlbumPeak: true}
	for _, n := range slices.Concat(meterTags, bpmTags, keyTags) {
		m[n] = true
	}
	return m
}

// maxMeterLen bounds the stored meter text.
const maxMeterLen = 32

// readAudioTags reads ReplayGain (or Opus R128) gains, the Opus output gain,
// the meter and the tempo of a file.
func readAudioTags(path, suffix string, meta tag.Metadata) audioTags {
	var out audioTags
	vals := collectTags(path, suffix, meta, audioTagAllow(), true)
	first := func(name string) (string, bool) {
		if v := vals[name]; len(v) > 0 {
			return v[0], true
		}
		return "", false
	}

	// Opus: R128 gains are authoritative (Q7.8 relative to -23 LUFS; +5 dB
	// converts to the ReplayGain reference of -18 LUFS). Other formats, or
	// Opus files without them, use REPLAYGAIN_* tags.
	if gain, ok := readOpusOutputGain(path); ok {
		out.BaseGain = sql.NullFloat64{Float64: gain, Valid: true}
	}
	if v, ok := first(r128Track); ok {
		out.TrackGain = parseR128(v)
	}
	if v, ok := first(r128Album); ok {
		out.AlbumGain = parseR128(v)
	}
	if !out.TrackGain.Valid {
		if v, ok := first(rgTrackGain); ok {
			out.TrackGain = parseNumber(v)
		}
	}
	if !out.AlbumGain.Valid {
		if v, ok := first(rgAlbumGain); ok {
			out.AlbumGain = parseNumber(v)
		}
	}
	if v, ok := first(rgTrackPeak); ok {
		out.TrackPeak = parseNumber(v)
	}
	if v, ok := first(rgAlbumPeak); ok {
		out.AlbumPeak = parseNumber(v)
	}

	for _, n := range meterTags {
		if v, ok := first(n); ok {
			if utf8.RuneCountInString(v) > maxMeterLen {
				v = string([]rune(v)[:maxMeterLen])
			}
			out.Meter = sql.NullString{String: v, Valid: true}
			break
		}
	}
	for _, n := range bpmTags {
		if v, ok := first(n); ok {
			if b := parseNumber(v); b.Valid && b.Float64 >= 20 && b.Float64 <= 400 {
				out.BPM = b
				break
			}
		}
	}
	for _, n := range keyTags {
		if v, ok := first(n); ok {
			if k, ok := analysis.ParseKey(v); ok {
				out.Key = k
				break
			}
		}
	}
	return out
}

// parseR128 converts an R128 gain tag (Q7.8 integer, -23 LUFS reference) to
// a ReplayGain value in dB (-18 LUFS reference).
func parseR128(s string) sql.NullFloat64 {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < -32768 || n > 32767 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: float64(n)/256 + 5, Valid: true}
}

// parseNumber reads the leading number of values like "-3.21 dB", "0.988",
// "120" or "120 BPM".
func parseNumber(s string) sql.NullFloat64 {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && strings.ContainsRune("+-.,0123456789", rune(s[end])) {
		end++
	}
	f, err := strconv.ParseFloat(strings.Replace(s[:end], ",", ".", 1), 64)
	if err != nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: f, Valid: true}
}
