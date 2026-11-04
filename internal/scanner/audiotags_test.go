package scanner

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
)

func near(v sql.NullFloat64, want float64) bool {
	return v.Valid && math.Abs(v.Float64-want) < 1e-6
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// opusFile is an Ogg Opus header with the given output gain (Q7.8) and tags.
func opusFile(outputGain int16, entries ...string) []byte {
	head := append([]byte("OpusHead"), make([]byte, 11)...)
	head[8] = 1 // version
	binary.LittleEndian.PutUint16(head[16:18], uint16(outputGain))
	tags := append([]byte("OpusTags"), vorbisBlock(entries...)...)
	return append(oggPage(2, head), oggPage(0, tags)...)
}

func TestAudioTagsOpusR128(t *testing.T) {
	// R128 -1254/256 = -4.898 dB (-23 LUFS) → +0.102 dB at -18 LUFS; album 512 → 7.
	path := writeFile(t, "a.opus", opusFile(-384, "R128_TRACK_GAIN=-1254", "R128_ALBUM_GAIN=512",
		"REPLAYGAIN_TRACK_GAIN=-9 dB", "METER=3/4", "BPM=165"))
	got := readAudioTags(path, "opus", nil)
	if !near(got.TrackGain, -1254.0/256+5) || !near(got.AlbumGain, 7) {
		t.Fatalf("R128 gains: %+v %+v (R128 must win over REPLAYGAIN in Opus)", got.TrackGain, got.AlbumGain)
	}
	if !near(got.BaseGain, -1.5) {
		t.Fatalf("output gain: %+v, want -1.5 dB", got.BaseGain)
	}
	if got.TrackPeak.Valid || got.AlbumPeak.Valid {
		t.Fatal("Opus R128 has no peaks")
	}
	if got.Meter.String != "3/4" || !near(got.BPM, 165) {
		t.Fatalf("meter %+v, bpm %+v", got.Meter, got.BPM)
	}
}

func TestAudioTagsFLACReplayGain(t *testing.T) {
	path := writeFile(t, "a.flac", flacWithComments(
		"REPLAYGAIN_TRACK_GAIN=-3.21 dB", "REPLAYGAIN_ALBUM_GAIN=+1.5 dB",
		"REPLAYGAIN_TRACK_PEAK=0.988", "REPLAYGAIN_ALBUM_PEAK=1.000",
		"TIME_SIGNATURE=  6/8  ", "BPM=not a number"))
	got := readAudioTags(path, "flac", nil)
	if !near(got.TrackGain, -3.21) || !near(got.AlbumGain, 1.5) || !near(got.TrackPeak, 0.988) || !near(got.AlbumPeak, 1) {
		t.Fatalf("gains/peaks: %+v", got)
	}
	if got.BaseGain.Valid {
		t.Fatal("FLAC has no base gain")
	}
	if got.Meter.String != "6/8" || got.BPM.Valid {
		t.Fatalf("meter %+v (trimmed), bpm %+v (invalid → unknown)", got.Meter, got.BPM)
	}
}

func TestAudioTagsMissing(t *testing.T) {
	path := writeFile(t, "a.flac", flacWithComments("TITLE=x"))
	if got := readAudioTags(path, "flac", nil); got != (audioTags{}) {
		t.Fatalf("no tags: %+v", got)
	}
}

// id3Frame is an ID3v2.3 frame.
func id3Frame(id string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(body)))
	b.Write([]byte{0, 0})
	b.Write(body)
	return b.Bytes()
}

// mp3WithID3 is an ID3v2.3 tag (ISO-8859-1 text) followed by filler.
func mp3WithID3(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	n := len(body)
	var b bytes.Buffer
	b.WriteString("ID3")
	b.Write([]byte{3, 0, 0})
	b.Write([]byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}) // syncsafe
	b.Write(body)
	b.Write(make([]byte, 64))
	return b.Bytes()
}

func TestAudioTagsMP3(t *testing.T) {
	txxx := func(desc, val string) []byte {
		return id3Frame("TXXX", append(append([]byte{0}, desc+"\x00"...), val...))
	}
	path := writeFile(t, "a.mp3", mp3WithID3(
		id3Frame("TIT2", append([]byte{0}, "Song"...)),
		id3Frame("TBPM", append([]byte{0}, "96"...)),
		txxx("REPLAYGAIN_TRACK_GAIN", "-6.5 dB"),
		txxx("replaygain_track_peak", "0.5"),
		txxx("METER", "4/4"),
	))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := tag.ReadFrom(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := readAudioTags(path, "mp3", meta)
	if !near(got.TrackGain, -6.5) || !near(got.TrackPeak, 0.5) || got.Meter.String != "4/4" || !near(got.BPM, 96) {
		t.Fatalf("mp3: %+v", got)
	}
}

func TestParseR128AndNumbers(t *testing.T) {
	for in, want := range map[string]float64{"0": 5, "-1280": 0, "256": 6, "-32768": -123} {
		if got := parseR128(in); !near(got, want) {
			t.Errorf("parseR128(%q) = %+v, want %v", in, got, want)
		}
	}
	if parseR128("1.5").Valid || parseR128("99999").Valid {
		t.Error("invalid R128 accepted")
	}
	for in, want := range map[string]float64{"-3.21 dB": -3.21, "+2 dB": 2, "120 BPM": 120, " 0,95 ": 0.95} {
		if got := parseNumber(in); !near(got, want) {
			t.Errorf("parseNumber(%q) = %+v, want %v", in, got, want)
		}
	}
	if parseNumber("dB").Valid {
		t.Error("no number accepted")
	}
}
