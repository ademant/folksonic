package config

import (
	"strings"
	"testing"
)

func TestWorkerToolValidation(t *testing.T) {
	ok := Analysis{BPM: "folksentia", Tools: map[string]AnalysisTool{"folksentia": {Worker: true}}}
	if err := ok.normalise(true); err != nil {
		t.Fatalf("worker tool without command: %v", err)
	}
	if !ok.Tools["folksentia"].Worker {
		t.Error("worker flag lost")
	}
	for name, c := range map[string]struct {
		tools map[string]AnalysisTool
		want  string
	}{
		"command set": {
			map[string]AnalysisTool{"folksentia": {Worker: true, Command: "/usr/local/bin/folksentia"}},
			"command must not be set",
		},
		"not folksentia": {
			map[string]AnalysisTool{"ffmpeg": {Worker: true}},
			"only supported for folksentia",
		},
	} {
		a := Analysis{BPM: "folksentia", Tools: c.tools}
		if name == "not folksentia" {
			a.BPM = "ffmpeg"
		}
		err := a.normalise(true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
