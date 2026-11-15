package main

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/analysis"
)

func TestTempoReportOutput(t *testing.T) {
	conf := sql.NullFloat64{Float64: 0.8, Valid: true}
	rhythm := sql.NullString{String: `{"pulseSec":0.5,"barPulses":2,"barsPerMin":60,"barScore":0.5}`, Valid: true}
	rows := []analysis.TempoRow{
		{ID: "so-a", Artist: "A", Title: "Tab\there", BPM: sql.NullFloat64{Float64: 240, Valid: true},
			Source: sql.NullString{String: "folksentia", Valid: true}, Tempo: analysis.ParseTempo(conf, rhythm), Ratio: "2"},
		{ID: "so-b", Artist: "A", Title: "b", BPM: sql.NullFloat64{Float64: 120, Valid: true},
			Source: sql.NullString{String: "tag", Valid: true}, Tempo: analysis.ParseTempo(sql.NullFloat64{}, rhythm), Ratio: "1"},
		{ID: "so-c", Artist: "A", Title: "c", Ratio: analysis.RatioUnknown},
	}
	var out bytes.Buffer
	if err := writeTempoRows(&out, rows, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || lines[0] != strings.Join(tempoColumns(), "\t") {
		t.Fatalf("tsv:\n%s", out.String())
	}
	if want := "so-a\t240.0\tfolksentia\t0.80\t60.00\t2\t120.0\t0.50\t2\tA\tTab here"; lines[1] != want {
		t.Fatalf("row\n%q\nwant\n%q", lines[1], want)
	}
	if !strings.HasPrefix(lines[3], "so-c\t-\t-\t-\t-\t-\t-\t-\t-\t") {
		t.Fatalf("empty row %q", lines[3])
	}

	out.Reset()
	if err := writeTempoSummary(&out, rows); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "3 songs: 2 with bpm, 2 with a measured pulse, 2 with both\n") {
		t.Fatalf("summary:\n%s", s)
	}
	for _, want := range []string{"folksentia", "tag", "1 (50%)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary lacks %q:\n%s", want, s)
		}
	}
}
