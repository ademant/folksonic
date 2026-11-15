package analysis

import (
	"context"
	"database/sql"
	"testing"
)

func TestTempoRatio(t *testing.T) {
	for _, c := range []struct {
		bpm, pulse float64
		want       string
	}{
		{120, 120, "1"},
		{124, 120, "1"},        // 3.3%: within 4%
		{126, 120, RatioOther}, // 5%
		{240, 120, "2"},
		{60, 120, "1/2"},
		{180, 120, "3/2"},
		{80, 120, "2/3"},
		{0, 120, RatioUnknown},
		{120, 0, RatioUnknown},
	} {
		if got := TempoRatio(c.bpm, c.pulse); got != c.want {
			t.Errorf("TempoRatio(%v, %v) = %q, want %q", c.bpm, c.pulse, got, c.want)
		}
	}
}

func TestTempoObject(t *testing.T) {
	if obj := ParseTempo(sql.NullFloat64{}, sql.NullString{}).Object(); obj != nil {
		t.Fatalf("nothing known: %v", obj)
	}
	if obj := ParseTempo(sql.NullFloat64{}, sql.NullString{String: "not json", Valid: true}).Object(); obj != nil {
		t.Fatalf("broken rhythm: %v", obj)
	}
	tv := ParseTempo(sql.NullFloat64{Float64: 0.5, Valid: true},
		sql.NullString{String: `{"pulseSec":0.5,"barPulses":3,"barsPerMin":40,"barScore":0.3}`, Valid: true})
	obj := tv.Object()
	if len(obj) != len(TempoKeys) || obj["confidence"] != 0.5 || obj["barsPerMin"] != 40.0 || obj["barPulses"] != 3 {
		t.Fatalf("object: %v", obj)
	}
	if p := tv.PulsePerMin(); p != 120 {
		t.Fatalf("pulse rate %v", p)
	}
}

func TestTempoReport(t *testing.T) {
	pool, _ := library(t, "a", "b", "c")
	if _, err := pool.Exec(`UPDATE songs SET bpm = 240, bpm_source = 'folksentia', bpm_confidence = 0.8 WHERE id = 'so-a';
		UPDATE songs SET bpm = 100, bpm_source = 'tag' WHERE id = 'so-b';
		INSERT INTO song_values (song_id, name, value, changed_ms) VALUES
			('so-a', 'rhythm', '{"pulseSec":0.5,"barPulses":2,"barsPerMin":60,"barScore":0.5}', 1),
			('so-c', 'rhythm', '{"pulseSec":0.5,"barPulses":2,"barsPerMin":60,"barScore":0.5}', 1)`); err != nil {
		t.Fatal(err)
	}
	rows, err := TempoReport(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"so-a": "2", "so-b": RatioUnknown, "so-c": RatioUnknown}
	if len(rows) != len(want) {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		if r.Ratio != want[r.ID] {
			t.Errorf("%s: ratio %q, want %q", r.ID, r.Ratio, want[r.ID])
		}
	}
	if r := rows[0]; r.ID != "so-a" || r.Source.String != "folksentia" || r.Tempo.Confidence == nil || *r.Tempo.Confidence != 0.8 {
		t.Fatalf("first row: %+v", r)
	}
}
