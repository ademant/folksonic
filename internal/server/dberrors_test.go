package server

import (
	"net/http"
	"strings"
	"testing"
)

// dropTable makes every query on the table fail, the way a database error
// would. Foreign keys are switched off for the one connection, so the drop is
// not refused by rows that refer to the table.
func dropTable(t *testing.T, s *State, name string) {
	t.Helper()
	s.Pool.SetMaxOpenConns(1)
	if _, err := s.Pool.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("DROP TABLE " + name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
}

// #110: a database error must reach the client as status failed, never as an
// ok response with an empty list. A sync that reads "empty" can delete data.
func TestDatabaseErrorsAreFailuresNotEmptyLists(t *testing.T) {
	cases := []struct {
		name, endpoint, table string
	}{
		{"search3", "search3.view?query=a", "artists"},
		{"getAlbum", "getAlbum.view?id=al-1", "albums"},
		{"getAlbumList2", "getAlbumList2.view?type=newest", "albums"},
		{"getArtists", "getArtists.view", "artists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, key := keyFixture(t)
			dropTable(t, s, c.table)
			rec := getWith(t, requestLogger(s.routes()), "/rest/"+c.endpoint, key, true)
			body := rec.Body.String()
			if !strings.Contains(body, `"status":"failed"`) || !strings.Contains(body, `"code":0`) {
				t.Fatalf("%s: want failed with error 0, got %d %s", c.name, rec.Code, body)
			}
			if strings.Contains(body, `"status":"ok"`) {
				t.Fatalf("%s: answered ok despite the database error: %s", c.name, body)
			}
		})
	}
}

// The cover endpoint answers 500, not 404, when the lookup fails, so the client
// does not cache "no cover".
func TestCoverArtDatabaseErrorIsServerError(t *testing.T) {
	s, _, key := keyFixture(t)
	dropTable(t, s, "albums")
	rec := getWith(t, requestLogger(s.routes()), "/rest/getCoverArt.view?id=al-1", key, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("cover on database error: %d, want 500", rec.Code)
	}
}
