package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The fail2ban filter matches the attack reasons only, not routine refusals
// such as an expired key (#186).
func TestFail2banFilterMatchesAttacksOnly(t *testing.T) {
	b, err := os.ReadFile("../../deploy/fail2ban/filter.d/tinysonic.conf")
	if err != nil {
		t.Fatal(err)
	}
	var expr string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "failregex = "); ok {
			expr = v
		}
	}
	if expr == "" {
		t.Fatal("no failregex")
	}
	re := regexp.MustCompile(strings.ReplaceAll(expr, "<HOST>", `(?P<host>\S+)`))
	for reason, want := range map[string]bool{
		"password": true, "unknown-user": true, "rate-limit": true, "key_unknown": true,
		"key_expired": false, "key_idle": false, "key_revoked": false, "session_expired": false,
		"session_unknown": false, "not-active": false, "busy": false, "user_disabled": false,
	} {
		line := "Nov 26 10:00:00 host tinysonic[1]: WARN auth failed ip=198.51.100.7 reason=" + reason + " path=/rest/ping"
		if got := re.MatchString(line); got != want {
			t.Errorf("reason=%s: matched %v, want %v", reason, got, want)
		}
	}
}
