package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// pagingFixture: 3 artists, 3 albums, 5 songs (two share a title, to check
// tie-breaking), all in genre "Folk".
func pagingFixture(t *testing.T) *State {
	t.Helper()
	s := newTestState(t)
	s.MusicDirs = []string{"/m"}
	for _, q := range []string{
		`INSERT INTO artists (id, name, name_lower) VALUES ('ar-1','Alpha','alpha'),('ar-2','Les Bravo','les bravo'),('ar-3','Charlie','charlie')`,
		`INSERT INTO albums (id, title, artist, artist_id, year) VALUES ('al-1','One','Alpha','ar-1',2001),('al-2','Two','Alpha','ar-1',2002),('al-3','Three','Charlie','ar-3',2003)`,
		`INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, genre, track_number, suffix, content_type, duration_ms, created_at) VALUES
			('so-1','/m/1.mp3','Same','Alpha','ar-1','One','al-1','Folk',1,'mp3','audio/mpeg',1000,'2026-01-01T00:00:00Z'),
			('so-2','/m/2.mp3','Same','Alpha','ar-1','One','al-1','Folk',2,'mp3','audio/mpeg',1000,'2026-01-01T00:00:00Z'),
			('so-3','/m/3.mp3','Same','Alpha','ar-1','Two','al-2','Folk',1,'mp3','audio/mpeg',1000,'2026-01-01T00:00:00Z'),
			('so-4','/m/4.mp3','Dance','Charlie','ar-3','Three','al-3','Folk',1,'mp3','audio/mpeg',1000,'2026-01-01T00:00:00Z'),
			('so-5','/m/5.mp3','Waltz','Charlie','ar-3','Three','al-3','Folk',2,'mp3','audio/mpeg',1000,'2026-01-01T00:00:00Z')`,
	} {
		if _, err := s.Pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type resp struct {
	code   int
	header http.Header
	body   string
	data   map[string]any
}

func call(t *testing.T, s *State, method, endpoint string, q url.Values, hdr map[string]string) resp {
	t.Helper()
	q.Set("u", "admin")
	q.Set("p", "s3cret")
	req := httptest.NewRequest(method, "/rest/"+endpoint+"?"+q.Encode(), nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	out := resp{code: rec.Code, header: rec.Header(), body: rec.Body.String()}
	if method != http.MethodHead {
		var env map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: %v: %s", endpoint, err, out.body)
		}
		out.data = env["subsonic-response"]
		if out.data["status"] != "ok" {
			t.Fatalf("%s: %s", endpoint, out.body)
		}
	}
	return out
}

func ids(list any) []string {
	var out []string
	if list == nil {
		return out
	}
	for _, e := range list.([]any) {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

func indexIDs(index any) []string {
	var out []string
	for _, g := range index.([]any) {
		out = append(out, ids(g.(map[string]any)["artist"])...)
	}
	return out
}

func TestPageParams(t *testing.T) {
	for q, want := range map[string]page{
		"":                    {},
		"size=10":             {10, 0, true},
		"offset=5":            {maxPageSize, 5, true},
		"size=1000&offset=-3": {maxPageSize, 0, true},
		"size=0":              {1, 0, true},
	} {
		got := pageParams(httptest.NewRequest(http.MethodGet, "/?"+q, nil))
		if got != want {
			t.Errorf("%q: %+v, want %+v", q, got, want)
		}
	}
	items := []int{0, 1, 2, 3, 4}
	if got := pageSlice(items, page{2, 4, true}); len(got) != 1 || got[0] != 4 {
		t.Errorf("last page: %v", got)
	}
	if got := pageSlice(items, page{2, 9, true}); len(got) != 0 {
		t.Errorf("past the end: %v", got)
	}
	if got := pageSlice(items, page{}); len(got) != 5 {
		t.Errorf("unset: %v", got)
	}
}

// Without size/offset, list endpoints return everything, as before; the
// total header is always present.
func TestPagingDefaultsUnchanged(t *testing.T) {
	s := pagingFixture(t)
	r := call(t, s, "GET", "getArtists", url.Values{}, nil)
	if got := indexIDs(r.data["artists"].(map[string]any)["index"]); strings.Join(got, ",") != "ar-1,ar-2,ar-3" {
		t.Fatalf("getArtists: %v", got)
	}
	if r.header.Get(hdrTotal) != "3" {
		t.Fatalf("X-Total-Count = %q", r.header.Get(hdrTotal))
	}
	r = call(t, s, "GET", "getAlbum", url.Values{"id": {"al-1"}}, nil)
	if got := ids(r.data["album"].(map[string]any)["song"]); len(got) != 2 {
		t.Fatalf("getAlbum songs: %v", got)
	}
	// getAlbumList2 keeps its standard default of 10.
	r = call(t, s, "GET", "getAlbumList2", url.Values{}, nil)
	if got := ids(r.data["albumList2"].(map[string]any)["album"]); len(got) != 3 || r.header.Get(hdrTotal) != "3" {
		t.Fatalf("getAlbumList2: %v, total %q", got, r.header.Get(hdrTotal))
	}
}

// Paging through a list with size=1 visits every item exactly once, in the
// unpaged order, including items with equal sort keys.
func TestPagingVisitsEveryItemOnce(t *testing.T) {
	s := pagingFixture(t)
	collect := func(endpoint string, q url.Values, extract func(map[string]any) []string) (all, paged []string) {
		all = extract(call(t, s, "GET", endpoint, q, nil).data)
		for off := 0; off < 10; off++ {
			pq := url.Values{}
			for k, v := range q {
				pq[k] = v
			}
			pq.Set("size", "1")
			pq.Set("offset", string(rune('0'+off)))
			got := extract(call(t, s, "GET", endpoint, pq, nil).data)
			if len(got) == 0 {
				break
			}
			if len(got) != 1 {
				t.Fatalf("%s offset %d: %v", endpoint, off, got)
			}
			paged = append(paged, got...)
		}
		return
	}
	for _, tc := range []struct {
		endpoint string
		q        url.Values
		extract  func(map[string]any) []string
	}{
		{"getArtists", url.Values{}, func(d map[string]any) []string { return indexIDs(d["artists"].(map[string]any)["index"]) }},
		{"getIndexes", url.Values{}, func(d map[string]any) []string { return indexIDs(d["indexes"].(map[string]any)["index"]) }},
		{"getArtist", url.Values{"id": {"ar-1"}}, func(d map[string]any) []string { return ids(d["artist"].(map[string]any)["album"]) }},
		{"getAlbum", url.Values{"id": {"al-1"}}, func(d map[string]any) []string { return ids(d["album"].(map[string]any)["song"]) }},
		{"getMusicDirectory", url.Values{"id": {"1"}}, func(d map[string]any) []string { return ids(d["directory"].(map[string]any)["child"]) }},
		{"getSongsByGenre", url.Values{"genre": {"Folk"}}, func(d map[string]any) []string { return ids(d["songsByGenre"].(map[string]any)["song"]) }},
		{"search3", url.Values{"query": {""}, "artistCount": {"0"}, "albumCount": {"0"}}, func(d map[string]any) []string {
			return ids(d["searchResult3"].(map[string]any)["song"])
		}},
	} {
		all, paged := collect(tc.endpoint, tc.q, tc.extract)
		if strings.Join(all, ",") != strings.Join(paged, ",") || len(all) == 0 {
			t.Errorf("%s: unpaged %v, paged %v", tc.endpoint, all, paged)
		}
	}
}

// search3 and getSongsByGenre: the standard parameters win over the size and
// offset aliases; the aliases apply to lists without a parameter of their own.
func TestPagingAliases(t *testing.T) {
	s := pagingFixture(t)
	r := call(t, s, "GET", "search3", url.Values{"query": {""}, "size": {"2"}, "offset": {"1"}, "songCount": {"4"}}, nil)
	res := r.data["searchResult3"].(map[string]any)
	if a, b, c := ids(res["artist"]), ids(res["album"]), ids(res["song"]); len(a) != 2 || len(b) != 2 || len(c) != 4 {
		t.Fatalf("aliases: artists %v albums %v songs %v", a, b, c)
	}
	// Search sorts by the plain name: Alpha, Charlie, Les Bravo.
	if ids(res["artist"])[0] != "ar-3" {
		t.Fatalf("offset alias not applied: %v", ids(res["artist"]))
	}
	if r.header.Get(hdrTotalArtist) != "3" || r.header.Get(hdrTotalAlbum) != "3" || r.header.Get(hdrTotalSong) != "5" {
		t.Fatalf("totals: %v", r.header)
	}
	r = call(t, s, "GET", "search3", url.Values{"query": {"same"}}, nil)
	if r.header.Get(hdrTotalSong) != "3" || r.header.Get(hdrTotalArtist) != "0" {
		t.Fatalf("search totals: song %q artist %q", r.header.Get(hdrTotalSong), r.header.Get(hdrTotalArtist))
	}
	r = call(t, s, "GET", "getSongsByGenre", url.Values{"genre": {"folk"}, "size": {"2"}}, nil)
	if got := ids(r.data["songsByGenre"].(map[string]any)["song"]); len(got) != 2 || r.header.Get(hdrTotal) != "5" {
		t.Fatalf("getSongsByGenre size alias: %v total %q", got, r.header.Get(hdrTotal))
	}
	r = call(t, s, "GET", "getSongsByGenre", url.Values{"genre": {"folk"}, "size": {"2"}, "count": {"3"}}, nil)
	if got := ids(r.data["songsByGenre"].(map[string]any)["song"]); len(got) != 3 {
		t.Fatalf("count must win over size: %v", got)
	}
}

// HEAD returns the total headers and no body.
func TestHeadReturnsTotalsOnly(t *testing.T) {
	s := pagingFixture(t)
	for endpoint, q := range map[string]url.Values{
		"getArtists":      {},
		"getAlbumList2":   {"type": {"newest"}},
		"getSongsByGenre": {"genre": {"Folk"}},
		"getRandomSongs":  {},
		"getAlbum":        {"id": {"al-3"}},
		"getPlaylists":    {},
		"getGenres":       {},
	} {
		r := call(t, s, http.MethodHead, endpoint, q, nil)
		if r.code != 200 || r.body != "" || r.header.Get(hdrTotal) == "" {
			t.Errorf("HEAD %s: code %d, body %q, total %q", endpoint, r.code, r.body, r.header.Get(hdrTotal))
		}
	}
	r := call(t, s, http.MethodHead, "search3", url.Values{"query": {"waltz"}}, nil)
	if r.body != "" || r.header.Get(hdrTotalSong) != "1" {
		t.Errorf("HEAD search3: body %q, songs %q", r.body, r.header.Get(hdrTotalSong))
	}
	r = call(t, s, http.MethodHead, "getStarred2", url.Values{}, nil)
	if r.body != "" || r.header.Get(hdrTotalSong) != "0" {
		t.Errorf("HEAD getStarred2: body %q, songs %q", r.body, r.header.Get(hdrTotalSong))
	}
}

func TestLibraryInfo(t *testing.T) {
	s := pagingFixture(t)
	info := call(t, s, "GET", "tinysonicLibraryInfo", url.Values{}, nil).data["tinysonicLibraryInfo"].(map[string]any)
	if info["artists"] != 3.0 || info["albums"] != 3.0 || info["songs"] != 5.0 || info["duration"] != 5.0 {
		t.Fatalf("counts: %v", info)
	}
	// No change recorded yet: falls back to the newest song.
	if info["lastModified"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("lastModified fallback: %v", info["lastModified"])
	}
	if _, err := s.Pool.Exec(`INSERT INTO settings (key, value) VALUES ('library_modified', '2026-11-03T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	info = call(t, s, "GET", "tinysonicLibraryInfo", url.Values{}, nil).data["tinysonicLibraryInfo"].(map[string]any)
	if info["lastModified"] != "2026-11-03T12:00:00Z" {
		t.Fatalf("lastModified: %v", info["lastModified"])
	}
}

func TestCORSExposesPagingHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	corsMiddleware(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, h := range []string{hdrTotal, hdrTotalSong, "Preference-Applied"} {
		if !strings.Contains(exposed, h) {
			t.Errorf("%s not exposed: %q", h, exposed)
		}
	}
}
