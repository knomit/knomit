package web

import (
	"encoding/json"
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

type probeOriginRequest struct {
	URL        string `json:"url"`
	Branch     string `json:"branch"`
	AuthMethod string `json:"auth_method"`
	AuthToken  string `json:"auth_token"`
}

// handleReposProbeOrigin serves POST /api/v1/repos:probe-origin. Collection
// level, because it runs BEFORE any repo exists — the origin-session test
// endpoint needs a {repo} and so cannot serve pre-create probing.
//
// Unreachable and auth-required come back as 200 results, not HTTP errors:
// both are states the wizard renders and recovers from. Only a refused
// request (the local-origin gate) is a 4xx.
func handleReposProbeOrigin(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req probeOriginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", err.Error(), r.URL.Path)
			return
		}
		if req.URL == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", "url is required", r.URL.Path)
			return
		}
		res, err := m.ProbeOrigin(r.Context(), repos.OriginSpec{
			URL:        req.URL,
			Branch:     req.Branch,
			AuthMethod: req.AuthMethod,
			AuthToken:  req.AuthToken,
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Origin rejected", err.Error(), r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}
