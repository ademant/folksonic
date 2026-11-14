package server

import (
	"net/http/httptest"
	"testing"
)

// #114: a list's own count parameter is clamped to 0..maxPageSize, so one
// request cannot ask for the whole library, and -1 is not "no limit".
func TestListCountsAreClamped(t *testing.T) {
	cases := []struct {
		query string
		want  int64
	}{
		{"songCount=100000", maxPageSize},
		{"songCount=-1", 0},
		{"songCount=0", 0},
		{"songCount=37", 37},
		{"", 20},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/rest/search3?"+c.query, nil)
		if got := countParam(r, "songCount", 20); got != c.want {
			t.Errorf("%q: countParam = %d, want %d", c.query, got, c.want)
		}
	}
}
