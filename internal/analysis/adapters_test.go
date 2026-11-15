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
		{`{"schema":3}`, "schema 3"},
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
	// rhythm and spectral are always in the output; onsets is opt-in.
	if got := folksentiaArgs([]string{"onsets", "rhythm", "spectral"}, "/a.mp3"); !reflect.DeepEqual(got, []string{"--onsets", "--", "/a.mp3"}) {
		t.Fatalf("%q", got)
	}
}

// folksentiaPlayerSample is folksentia output with --onsets
// (ademant/folksentia#42), the rhythm object and the spectral descriptors.
const folksentiaPlayerSample = `{"schema":1,"file":"t.flac","duration":200.5,"bpm":124.0,
 "spectral_complexity":12.5,"spectral_entropy":7.31,
 "onsets":{"schema":1,"timesMs":[512,768,1023],"strengths":[255,96,140]},
 "rhythm":{"schema":1,"pulseSec":0.527,"barPulses":3,"barSec":1.581,"barsPerMin":38,"barScore":0.34,
   "alternatives":[{"pulses":6,"sec":3.161,"score":0.23}],"accents":{"energy":[1,0.57,0.41],"bass":[0.73,1,0.87]},
   "tempoCurve":[{"startSec":0.0,"barsPerMin":38}]}}`

func TestParseFolksentiaOnsetsRhythmSpectral(t *testing.T) {
	features := []string{"onsets", "rhythm", "spectral"}
	v, err := parseFolksentia([]byte(folksentiaPlayerSample), features)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(v)
	want := `{"onsets":{"timesMs":[512,768,1023],"strengths":[255,96,140]},` +
		`"rhythm":{"pulseSec":0.527,"barPulses":3,"barsPerMin":38,"barScore":0.34},` +
		`"spectral":{"complexity":12.5,"entropy":7.31}}`
	if string(js) != want {
		t.Fatalf("got  %s\nwant %s", js, want)
	}

	// No onset above the threshold is known ([]); null, a missing rhythm
	// and an older folksentia without spectral fields are unknown.
	v, err = parseFolksentia([]byte(`{"schema":1,"onsets":{"schema":1,"timesMs":[],"strengths":[]},"rhythm":null}`), features)
	if err != nil {
		t.Fatal(err)
	}
	if js, _ := json.Marshal(v); string(js) != `{"onsets":{"timesMs":[],"strengths":[]}}` {
		t.Fatalf("empty onsets: %s", js)
	}
	v, err = parseFolksentia([]byte(`{"schema":1,"onsets":null,"spectral_complexity":null,"spectral_entropy":null}`), features)
	if err != nil || len(v) != 0 {
		t.Fatalf("nulls: %v %v", v, err)
	}
	v, err = parseFolksentia([]byte(folksentiaSample), features)
	if err != nil || len(v) != 0 {
		t.Fatalf("output without these fields: %v %v", v, err)
	}

	for _, c := range []struct{ out, want string }{
		{`{"schema":1,"onsets":{"schema":2,"timesMs":[],"strengths":[]}}`, "onsets schema 2"},
		{`{"schema":1,"onsets":{"schema":1,"timesMs":[1,2],"strengths":[3]}}`, "2 onset times but 1 strengths"},
		{`{"schema":1,"onsets":{"schema":1,"timesMs":[5,4],"strengths":[1,1]}}`, "out of order"},
		{`{"schema":1,"onsets":{"schema":1,"timesMs":[-1],"strengths":[1]}}`, "out of order"},
		{`{"schema":1,"onsets":{"schema":1,"timesMs":[1],"strengths":[256]}}`, "out of range"},
		{`{"schema":1,"rhythm":{"schema":2,"barsPerMin":38}}`, "rhythm schema 2"},
		{`{"schema":1,"rhythm":{"schema":1,"pulseSec":0.5,"barPulses":0,"barsPerMin":38}}`, "impossible rhythm"},
		{`{"schema":1,"rhythm":{"schema":1,"pulseSec":-0.5,"barPulses":2,"barsPerMin":38}}`, "impossible rhythm"},
	} {
		if _, err := parseFolksentia([]byte(c.out), features); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.out, err, c.want)
		}
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

// Schema 2 reports the key as key_pitch and key_mode; the stored key is the
// same text schema 1 gives (#125).
func TestParseFolksentiaSchema2Key(t *testing.T) {
	key := func(out string) string {
		t.Helper()
		v, err := parseFolksentia([]byte(out), []string{"key"})
		if err != nil {
			t.Fatal(err)
		}
		js, _ := json.Marshal(v["key"])
		return string(js)
	}
	base := `{"schema":2,"file":"t.flac","key_strength":0.81,%s}`
	if got := key(strings.Replace(base, "%s", `"key_pitch":6,"key_mode":1`, 1)); got != `{"tonic":"F#","mode":"minor","confidence":0.81}` {
		t.Fatalf("F# minor: %s", got)
	}
	if got := key(strings.Replace(base, "%s", `"key_pitch":0,"key_mode":0`, 1)); got != `{"tonic":"C","mode":"major","confidence":0.81}` {
		t.Fatalf("C major: %s", got)
	}
	if got := key(strings.Replace(base, "%s", `"key_pitch":null,"key_mode":null`, 1)); got != "null" {
		t.Fatalf("unknown key: %s", got)
	}
	if got := key(strings.Replace(base, "%s", `"key_pitch":12,"key_mode":0`, 1)); got != "null" {
		t.Fatalf("pitch out of range: %s", got)
	}
	if got := key(strings.Replace(base, "%s", `"key_pitch":3,"key_mode":2`, 1)); got != "null" {
		t.Fatalf("mode out of range: %s", got)
	}
}
