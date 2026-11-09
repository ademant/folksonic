package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	ok := map[string]int64{
		"1024": 1024,
		"500M": 500 << 20,
		"2g":   2 << 30,
		"64k":  64 << 10,
		" 1G ": 1 << 30,
	}
	for in, want := range ok {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-5M", "10T", "M", "12MB", "99999999999G"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) accepted", in)
		}
	}
}

func TestToolLimitsAreParsedAndValidated(t *testing.T) {
	a := Analysis{
		BPM: "folksentia",
		Tools: map[string]AnalysisTool{"folksentia": {
			Command: "/usr/local/bin/folksentia", MemoryLimit: "2G", MaxDuration: "45m",
		}},
	}
	if err := a.normalise(true); err != nil {
		t.Fatal(err)
	}
	tool := a.Tools["folksentia"]
	if tool.MemoryLimitBytes != 2<<30 || tool.MaxDurationDur != 45*time.Minute {
		t.Errorf("parsed limits: %d bytes, %v", tool.MemoryLimitBytes, tool.MaxDurationDur)
	}

	for name, tool := range map[string]AnalysisTool{
		"memory_limit": {Command: "/usr/local/bin/folksentia", MemoryLimit: "lots"},
		"max_duration": {Command: "/usr/local/bin/folksentia", MaxDuration: "-1m"},
	} {
		a := Analysis{BPM: "folksentia", Tools: map[string]AnalysisTool{"folksentia": tool}}
		err := a.normalise(true)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %v", name, err)
		}
	}
}
