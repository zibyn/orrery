package api

import "net/http"

// POST /git/credential returns the Git credential a Sandbox Token may use for
// a host or provider. Issuing real Git tokens is the Git Provider ticket's job;
// until it lands this endpoint is a placeholder that always answers not_found
// (ticket #76, api.md §3).
func (s *Server) handleGitCredential(w http.ResponseWriter, r *http.Request, p *principal) {
	writeProblem(w, "not_found", "no Git credential is available")
}
