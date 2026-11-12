package server

import "net/http"

// openSubsonicExtensions lists the OpenSubsonic extensions this server really
// implements. Add a name only when its behaviour exists.
var baseOpenSubsonicExtensions = []J{
	{"name": "apiKeyAuthentication", "versions": []int{1}},       // apiKey parameter, see auth.go
	{"name": "formPost", "versions": []int{1}},                   // every endpoint accepts POST forms
	{"name": "tinysonicPaging", "versions": []int{1}},            // size/offset and X-Total-Count*, see paging.go
	{"name": "tinysonicLibraryInfo", "versions": []int{1}},       // tinysonicLibraryInfo endpoint
	{"name": "tinysonicMinimal", "versions": []int{1}},           // Prefer: tinysonic-minimal, see minimal.go
	{"name": "tinysonicMeter", "versions": []int{1}},             // tinysonicMeter on songs from METER/TIME_SIGNATURE tags
	{"name": "tinysonicLibraryChanges", "versions": []int{1, 2}}, // getLibraryChanges; 2 adds include=items, see changes.go
	{"name": "tinysonicAnalysis", "versions": []int{1, 2}},       // GET /api/analysis; 2 adds fingerprints and GET /api/duplicates
}

// GetOpenSubsonicExtensions lists the extensions above. It deliberately needs no
// sign-in: it reveals only what the server can do, and clients call it to find
// out which features to use.
func (s *State) GetOpenSubsonicExtensions(w http.ResponseWriter, r *http.Request) {
	list := append([]J{}, baseOpenSubsonicExtensions...)
	// Vendor extension: GET /api/stream-url mints signed stream URLs, see streamurl.go.
	list = append(list, J{"name": "tinysonicStreamUrl", "versions": []int{1}})
	if len(s.SongTags) > 0 {
		// Vendor extension: song responses carry tinysonicTags, see songToChild.
		list = append(list, J{"name": "tinysonicSongTags", "versions": []int{1}})
	}
	writeOK(w, J{"openSubsonicExtensions": list})
}
