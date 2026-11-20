package server

import "net/http"

// GetLicense answers getLicense. folksonic is free software with no license
// server, so the license is always valid and never expires (#141).
func (s *State) GetLicense(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"license": J{"valid": true}})
}

// GetTokenInfo answers tokenInfo for the API key that signed the request (#143).
// The OpenSubsonic apiKeyAuthentication extension defines only the username. A
// request without a key, or with one that is not valid, gets error 44.
func (s *State) GetTokenInfo(w http.ResponseWriter, r *http.Request) {
	if apiKeyOf(r) == "" {
		writeError(w, 44, "Invalid API key")
		return
	}
	u, ok := s.authenticate(r)
	if !ok {
		writeError(w, 44, "Invalid API key")
		return
	}
	writeOK(w, J{"tokenInfo": J{"username": u.Username}})
}
