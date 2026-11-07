package analysis

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// folksentiaSample is folksentia output (schema 1) with every option set.
const folksentiaSample = `{"schema":1,"file":"t.flac","duration":200.5,"bpm":124.0,"bpm_confidence":2.66,
 "key":"F#","scale":"minor","key_strength":0.81,"tuning_frequency":440,"tuning_cents":0,
 "replay_gain_db":-3,"loudness_db":-10,"dynamic_complexity":4,
 "silence":{"leadMs":350.4,"tailMs":1200},
 "loudness":{"integratedLufs":-9.4,"truePeakDbtp":-0.8},
 "waveform":{"bins":3,"peaks":[0,12,255]},
 "beats":[0.0,0.5,1.0005],"downbeatIndexes":[0],
 "fingerprint":{"algorithm":"chromaprint","version":2,"durationSec":120,"value":"AQAAAUkA"}}`

func TestParseFolksentia(t *testing.T) {
	all := []string{"bpm", "beats", "key", "loudness", "silence", "waveform", "fingerprint"}
	v, err := parseFolksentia([]byte(folksentiaSample), all)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(v)
	want := `{"beats":{"timesMs":[0,500,1001],"downbeatIndexes":[0]},` +
		`"bpm":{"value":124,"confidence":0.5},` +
		`"fingerprint":{"algorithm":"chromaprint","version":2,"durationSec":120,"value":"AQAAAUkA"},` +
		`"key":{"tonic":"F#","mode":"minor","confidence":0.81},` +
		`"loudness":{"integratedLufs":-9.4,"truePeakDbtp":-0.8},` +
		`"silence":{"leadMs":350,"tailMs":1200},` +
		`"waveform":{"bins":3,"peaks":[0,12,255]}}`
	if string(js) != want {
		t.Fatalf("got  %s\nwant %s", js, want)
	}

	// Only the requested features are returned.
	v, err = parseFolksentia([]byte(folksentiaSample), []string{"key"})
	if err != nil || len(v) != 1 || v["key"] == nil {
		t.Fatalf("key only: %v %v", v, err)
	}
}

func TestParseFolksentiaUnknownAndErrors(t *testing.T) {
	nulls := `{"schema":1,"file":"s.flac","bpm":null,"bpm_confidence":null,"key":null,"scale":null,"key_strength":null,
	  "silence":{"leadMs":null,"tailMs":null},"loudness":{"integratedLufs":null,"truePeakDbtp":null},
	  "beats":[],"downbeatIndexes":null,"fingerprint":null}`
	v, err := parseFolksentia([]byte(nulls), []string{"bpm", "key", "loudness", "silence", "beats", "waveform", "fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v["beats"] == nil {
		t.Fatalf("unknown values must be absent (beats empty but known): %v", v)
	}
	for _, c := range []struct{ out, want string }{
		{`{"schema":1,"file":"x","error":"no audio decoded from x"}`, "no audio decoded"},
		{`{"schema":2}`, "schema 2"},
		{`not json`, "reading output"},
		{`{"schema":1,"waveform":{"bins":1,"peaks":[300]}}`, "out of range"},
		{`{"schema":1,"beats":[0],"downbeatIndexes":[5]}`, "out of range"},
		{`{"schema":1,"fingerprint":{"algorithm":"x","version":1,"value":"AQAAAQE"}}`, "algorithm"},
		{`{"schema":1,"fingerprint":{"algorithm":"chromaprint","version":2,"value":"AQ"}}`, "fingerprint"},
	} {
		if _, err := parseFolksentia([]byte(c.out), []string{"waveform", "beats", "fingerprint"}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.out, err, c.want)
		}
	}
}

func TestFolksentiaArgs(t *testing.T) {
	got := folksentiaArgs([]string{"bpm", "beats", "waveform", "fingerprint"}, "-odd name.flac")
	want := []string{"--downbeats", "--waveform=150", "--fingerprint", "--", "-odd name.flac"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if got := folksentiaArgs([]string{"key"}, "/a.mp3"); !reflect.DeepEqual(got, []string{"--", "/a.mp3"}) {
		t.Fatalf("%q", got)
	}
}

func TestParseEBUR128(t *testing.T) {
	out := `[Parsed_ebur128_0 @ 0x1] Summary:

  Integrated loudness:
    I:         -27.8 LUFS
    Threshold: -37.8 LUFS

  Loudness range:
    LRA:         0.0 LU

  True peak:
    Peak:      -24.1 dBFS
`
	l, err := parseEBUR128([]byte("noise before\n" + out))
	if err != nil || l.IntegratedLufs == nil || *l.IntegratedLufs != -27.8 || l.TruePeakDbtp == nil || *l.TruePeakDbtp != -24.1 {
		t.Fatalf("%+v %v", l, err)
	}
	silent := strings.NewReplacer("-27.8 LUFS", "-70.0 LUFS", "-24.1 dBFS", "-inf dBFS").Replace(out)
	l, err = parseEBUR128([]byte(silent))
	if err != nil || l.IntegratedLufs != nil || l.TruePeakDbtp != nil {
		t.Fatalf("silence: %+v %v", l, err)
	}
	if _, err := parseEBUR128([]byte("Error opening input")); err == nil {
		t.Fatal("no summary accepted")
	}
}

func TestParseKey(t *testing.T) {
	for in, want := range map[string]string{
		"A minor": "A minor", "Am": "A minor", "am": "A minor", "F#m": "F# minor", "F♯ min": "F# minor",
		"Bb": "Bb major", "bb": "Bb major", "Eb major": "Eb major", "C": "C major", "Abm": "Ab minor",
		"D Dur": "D major", "g moll": "G minor", " E  ": "E major",
	} {
		k, ok := ParseKey(in)
		if !ok || k.Tonic+" "+k.Mode != want {
			t.Errorf("%q: %v %v, want %s", in, k, ok, want)
		}
	}
	for _, in := range []string{"", "8A", "1m", "H", "Cb", "A dorian", "Amaj7"} {
		if k, ok := ParseKey(in); ok {
			t.Errorf("%q accepted as %v", in, k)
		}
	}
}
