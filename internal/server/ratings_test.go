package server

import (
	"context"
	"strings"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/users"
)

// setRating answers the Subsonic envelope for one rating change.
func setRating(t *testing.T, s *State, key, query string) string {
	t.Helper()
	return getWith(t, s.routes(), "/rest/setRating.view?"+query, key, true).Body.String()
}

// #140: a rating is stored for the caller and shown as userRating on songs.
func TestSetRatingStoresForCaller(t *testing.T) {
	s, _, key := keyFixture(t)
	if body := setRating(t, s, key, "id=so-1&rating=4"); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("setRating: %s", body)
	}
	if body := songBody(t, s, key, "so-1"); !strings.Contains(body, `"userRating":4`) {
		t.Fatalf("getSong after rating: %s", body)
	}
}

// A second rating replaces the first; 0 removes it and userRating goes away.
func TestSetRatingReplacesAndZeroRemoves(t *testing.T) {
	s, _, key := keyFixture(t)
	setRating(t, s, key, "id=so-1&rating=2")
	setRating(t, s, key, "id=so-1&rating=5")
	if body := songBody(t, s, key, "so-1"); !strings.Contains(body, `"userRating":5`) {
		t.Fatalf("replaced rating: %s", body)
	}
	setRating(t, s, key, "id=so-1&rating=0")
	if body := songBody(t, s, key, "so-1"); strings.Contains(body, "userRating") {
		t.Fatalf("rating still present after 0: %s", body)
	}
}

// A rating is per user: another account does not see it.
func TestRatingIsPerUser(t *testing.T) {
	s, _, key := keyFixture(t)
	if _, err := users.Create(context.Background(), s.Pool, "carol", "pw", users.RoleUser, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	carol := newKeyFor(t, s, "carol")
	setRating(t, s, key, "id=so-1&rating=3")
	if body := songBody(t, s, carol, "so-1"); strings.Contains(body, "userRating") {
		t.Fatalf("another user sees the rating: %s", body)
	}
}

// Out-of-range, non-numeric and missing values are refused; an unknown song is not found.
func TestSetRatingRefusesBadInput(t *testing.T) {
	s, _, key := keyFixture(t)
	cases := map[string]string{
		"id=so-1&rating=6":    `"code":10`,
		"id=so-1&rating=-1":   `"code":10`,
		"id=so-1&rating=x":    `"code":10`,
		"id=so-1":             `"code":10`,
		"rating=3":            `"code":10`,
		"id=so-nope&rating=3": `"code":70`,
	}
	for q, want := range cases {
		if body := setRating(t, s, key, q); !strings.Contains(body, want) {
			t.Errorf("%s: %s, want %s", q, body, want)
		}
	}
}
