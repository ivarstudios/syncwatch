package web

import "net/http"

// handleLobby serves the lobby page: a read-only full-screen view for a
// screen in the office. Not available yet.
func (s *Server) handleLobby(w http.ResponseWriter, r *http.Request, info requestInfo) {
	http.NotFound(w, r)
}

// fragLobby serves the body of the lobby page for live updates. Not
// available yet.
func (s *Server) fragLobby(w http.ResponseWriter, r *http.Request, info requestInfo) {
	http.NotFound(w, r)
}
