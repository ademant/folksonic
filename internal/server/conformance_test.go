package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// restRaw sends a request to the router as the admin, with any method.
func restRaw(t *testing.T, s *State, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(method, target+sep+"u=admin&p=s3cret&f=json", nil))
	return rec
}

// #192: GET and POST everywhere; HEAD only where nothing changes; every other
// method 405 with Allow, in the Subsonic envelope.
func TestRestMethods(t *testing.T) {
	s := pagingFixture(t)
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, ep := range []string{"/rest/deletePlaylist.view?id=x", "/rest/ping", "/rest/star?id=so-1"} {
			rec := restRaw(t, s, m, ep)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Errorf("%s %s: %d Allow=%q %s", m, ep, rec.Code, rec.Header().Get("Allow"), rec.Body)
			}
		}
	}
	// HEAD on a mutation is refused and changes nothing.
	rec := restRaw(t, s, http.MethodHead, "/rest/star.view?id=so-1")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
		t.Errorf("HEAD star: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if body := restRaw(t, s, http.MethodGet, "/rest/getStarred2").Body.String(); strings.Contains(body, "so-1") {
		t.Errorf("HEAD starred a song: %s", body)
	}
	// HEAD on a read, GET and POST on a mutation work.
	if rec := restRaw(t, s, http.MethodHead, "/rest/ping.view"); rec.Code != http.StatusOK {
		t.Errorf("HEAD ping: %d", rec.Code)
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if body := restRaw(t, s, m, "/rest/star.view?id=so-1").Body.String(); !strings.Contains(body, `"status":"ok"`) {
			t.Errorf("%s star: %s", m, body)
		}
	}
}

// #193: an unknown /rest endpoint and cover-art misses stay in the envelope.
func TestRestErrorsStayInTheEnvelope(t *testing.T) {
	s := pagingFixture(t)
	for target, code := range map[string]string{
		"/rest/noSuchEndpoint.view": `"code":70`,
		"/rest/getCoverArt.view":    `"code":10`,
		"/rest/getCoverArt?id=al-1": `"code":70`,
		"/rest/getCoverArt?id=zz":   `"code":70`,
	} {
		rec := restRaw(t, s, http.MethodGet, target)
		if !strings.Contains(rec.Body.String(), `"subsonic-response"`) || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
}

// #193: /api answers a wrong method with JSON 405 and Allow, an unknown path
// with JSON 404, and a 401 with WWW-Authenticate.
func TestAPIMethodAndAuthErrors(t *testing.T) {
	s := pagingFixture(t)
	code, body := apiCall(t, s, "admin", http.MethodPut, "/api/keys", "")
	if code != http.StatusMethodNotAllowed || body["error"] != "method_not_allowed" {
		t.Errorf("PUT /api/keys: %d %v", code, body)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/keys", nil))
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD, POST" {
		t.Errorf("Allow %q", allow)
	}
	code, body = apiCall(t, s, "admin", http.MethodGet, "/api/no-such-thing", "")
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Errorf("unknown /api path: %d %v", code, body)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("401 without WWW-Authenticate: %d %v", rec.Code, rec.Header())
	}
}

// #190: getUser reports a librarian as admin and refuses another account
// to anyone else.
func TestGetUserRoles(t *testing.T) {
	s, _, key := keyFixture(t)
	if body := restRaw(t, s, http.MethodGet, "/rest/getUser").Body.String(); !strings.Contains(body, `"adminRole":true`) {
		t.Errorf("librarian: %s", body)
	}
	if body := restRaw(t, s, http.MethodGet, "/rest/getUser?username=bob").Body.String(); !strings.Contains(body, `"username":"bob"`) || !strings.Contains(body, `"adminRole":false`) {
		t.Errorf("librarian reading bob: %s", body)
	}
	if body := getWith(t, s.routes(), "/rest/getUser.view?username=admin", key, true).Body.String(); !strings.Contains(body, `"code":50`) {
		t.Errorf("user reading another account: %s", body)
	}
	if body := getWith(t, s.routes(), "/rest/getUser.view?username=bob", key, true).Body.String(); !strings.Contains(body, `"username":"bob"`) {
		t.Errorf("user reading themselves: %s", body)
	}
}

// #190: starred and highest are their own lists; bad numbers are error 10;
// size=0 is an empty page.
func TestAlbumListConformance(t *testing.T) {
	s := pagingFixture(t)
	restRaw(t, s, http.MethodGet, "/rest/star?albumId=al-2")
	restRaw(t, s, http.MethodGet, "/rest/setRating?id=so-4&rating=5")
	restRaw(t, s, http.MethodGet, "/rest/setRating?id=so-1&rating=2")
	list := func(q string) []string {
		body := restRaw(t, s, http.MethodGet, "/rest/getAlbumList2?"+q).Body.String()
		al, _ := subsonicBody(t, body)["albumList2"].(map[string]any)
		return idsIn(t, al["album"])
	}
	assertIDs(t, "starred", list("type=starred"), []string{"al-2"})
	assertIDs(t, "highest", list("type=highest"), []string{"al-3", "al-1"})
	assertIDs(t, "size=0", list("type=alphabeticalByName&size=0"), nil)
	for _, q := range []string{"getAlbumList2?type=newest&size=abc", "getRandomSongs?size=x", "search3?query=a&songCount=1.5"} {
		if body := restRaw(t, s, http.MethodGet, "/rest/"+q).Body.String(); errorCode(t, body) != 10 {
			t.Errorf("%s: %s", q, body)
		}
	}
}

// #195: search needs query; empty is the documented browse mode; a query with
// nothing searchable finds nothing.
func TestSearchQueryRules(t *testing.T) {
	s := pagingFixture(t)
	songs := func(q string) (int, []string) {
		body := restRaw(t, s, http.MethodGet, "/rest/search3?"+q).Body.String()
		sr := subsonicBody(t, body)
		res, _ := sr["searchResult3"].(map[string]any)
		return errorCode(t, body), idsIn(t, res["song"])
	}
	if code, _ := songs("songCount=10"); code != 10 {
		t.Errorf("missing query: code %d", code)
	}
	for _, q := range []string{"query=", "query=" + url.QueryEscape(`""`)} {
		if code, ids := songs(q + "&songCount=10"); code != -1 || len(ids) != 5 {
			t.Errorf("%s: code %d, %d songs", q, code, len(ids))
		}
	}
	if code, ids := songs("query=" + url.QueryEscape("?!") + "&songCount=10"); code != -1 || len(ids) != 0 {
		t.Errorf("punctuation only: code %d, %v", code, ids)
	}
	if _, ids := songs("query=waltz"); len(ids) != 1 {
		t.Errorf("real search: %v", ids)
	}
}

// #195: an oversized JSON body is 413, trailing data and the wrong type are
// refused.
func TestDecodeBodyErrors(t *testing.T) {
	s := pagingFixture(t)
	h := limitBody(s.routes())
	send := func(ct, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, decodeJSON(t, rec.Body.String())
	}
	big := `{"username":"` + strings.Repeat("a", 2<<20) + `"}`
	if code, body := send("application/json", big); code != http.StatusRequestEntityTooLarge || body["error"] != "too_large" {
		t.Errorf("oversized: %d %v", code, body)
	}
	if code, body := send("application/json", `{"username":"a"} {"x":1}`); code != http.StatusBadRequest || body["error"] != "invalid_json" {
		t.Errorf("trailing data: %d %v", code, body)
	}
	if code, body := send("text/plain", `{"username":"a"}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d %v", code, body)
	}
	if code, _ := send("application/json; charset=utf-8", `{"username":"admin","password":"s3cret"}`); code != http.StatusOK {
		t.Errorf("valid login with charset: %d", code)
	}
}

// #173: a failed playlist edit is reported and changes nothing.
func TestPlaylistEditFailureIsReported(t *testing.T) {
	s := pagingFixture(t)
	body := restRaw(t, s, http.MethodGet, "/rest/createPlaylist?name=Mine&songId=so-1&songId=so-2").Body.String()
	pl, _ := subsonicBody(t, body)["playlist"].(map[string]any)
	id, _ := pl["id"].(string)
	if id == "" || pl["public"] != false {
		t.Fatalf("createPlaylist: %s", body)
	}
	// Song inserts fail; the rename in the same call must not stay.
	if _, err := s.Pool.Exec(`CREATE TRIGGER no_adds BEFORE INSERT ON playlist_songs BEGIN SELECT RAISE(ABORT, 'nope'); END`); err != nil {
		t.Fatal(err)
	}
	body = restRaw(t, s, http.MethodGet, "/rest/updatePlaylist?playlistId="+id+"&name=Renamed&songIdToAdd=so-3").Body.String()
	if errorCode(t, body) != 0 || !strings.Contains(body, "database error") {
		t.Fatalf("failed update reported as %s", body)
	}
	got, err := repoFindPlaylist(context.Background(), s.Pool, userIDOf(t, s, "admin"), id)
	if err != nil || got == nil || got.Name != "Mine" {
		t.Fatalf("rename kept after a failed edit: %+v %v", got, err)
	}
	if body := restRaw(t, s, http.MethodGet, "/rest/createPlaylist?name=Other&songId=so-1").Body.String(); errorCode(t, body) != 0 {
		t.Fatalf("failed create reported as %s", body)
	}
	if n, _ := repoCount(context.Background(), s.Pool, "SELECT COUNT(*) FROM playlists WHERE name = 'Other'"); n != 0 {
		t.Fatal("a playlist was created although its songs failed")
	}
}

// #190: a taken username is username_taken, and probing is limited.
func TestRegistrationProbingIsLimited(t *testing.T) {
	s := pagingFixture(t)
	s.AllowRegistration = true
	var last int
	for i := 0; i < 20; i++ {
		code, body := apiCall(t, s, "", http.MethodPost, "/api/register", `{"username":"admin","password":"a long enough password"}`)
		last = code
		if i == 0 && (code != http.StatusConflict || body["error"] != "username_taken") {
			t.Fatalf("taken name: %d %v", code, body)
		}
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("probing was never limited: last %d", last)
	}
}
