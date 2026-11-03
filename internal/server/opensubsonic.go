package server

import "net/http"

// openSubsonicExtensions lists the OpenSubsonic extensions this server really
// implements. Add a name only when its behaviour exists.
var baseOpenSubsonicExtensions = []J{
	{"name": "apiKeyAuthentication", "versions": []int{1}}, // apiKey parameter, see auth.go
	{"name": "formPost", "versions": []int{1}},             // every endpoint accepts POST forms
}

// GetOpenSubsonicExtensions lists the extensions above. It deliberately needs no
// sign-in: it reveals only what the server can do, and clients call it to find
// out which features to use.
func (s *State) GetOpenSubsonicExtensions(w http.ResponseWriter, r *http.Request) {
	list := append([]J{}, baseOpenSubsonicExtensions...)
	if len(s.SongTags) > 0 {
		// Vendor extension: song responses carry tinysonicTags, see songToChild.
		list = append(list, J{"name": "tinysonicSongTags", "versions": []int{1}})
	}
	writeOK(w, J{"openSubsonicExtensions": list})
}
