package server

import "testing"

func TestFTS5Match(t *testing.T) {
	tests := map[string]string{
		"":                    "",
		"   ":                 "",
		"radiohead":           `"radiohead"*`,
		"pink floyd":          `"pink"* "floyd"*`,
		`"quoted" AND OR NOT`: `"quoted"* "AND"* "OR"* "NOT"*`,
		`a*b:c(d)`:            `"abcd"*`,
		"AC/DC":               `"ACDC"*`,
	}
	for in, want := range tests {
		if got := fts5Match(in); got != want {
			t.Errorf("fts5Match(%q) = %q, want %q", in, got, want)
		}
	}
}
