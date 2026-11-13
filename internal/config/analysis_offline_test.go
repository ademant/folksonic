package config

import (
	"strings"
	"testing"
)

// The network worker was removed (#105): worker = true is refused with a
// hint, and folksentia without a command is accepted (offline only).
func TestOfflineToolValidation(t *testing.T) {
	ok := Analysis{BPM: "folksentia", Tools: map[string]AnalysisTool{"folksentia": {}}}
	if err := ok.normalise(true); err != nil {
		t.Fatalf("folksentia without command: %v", err)
	}
	old := Analysis{BPM: "folksentia", Tools: map[string]AnalysisTool{"folksentia": {Worker: true}}}
	if err := old.normalise(true); err == nil || !strings.Contains(err.Error(), "analysis export") {
		t.Errorf("worker = true: %v", err)
	}
	rel := Analysis{BPM: "folksentia", Tools: map[string]AnalysisTool{"folksentia": {Command: "bin/folksentia"}}}
	if err := rel.normalise(true); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("relative command: %v", err)
	}
}
