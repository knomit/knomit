package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// handleCommitAnchoredTopicNode serves:
//
//	GET /repos/{repo}/branches/{branch}/commits/{sha}/topics
//	GET /repos/{repo}/branches/{branch}/commits/{sha}/topics/*
//	(including .../topics/{segments}/facts and .../topics/{segments}/stats)
//
// The store's ListDir only operates at branch HEAD; there is no API to list
// directory contents at an arbitrary commit. All of these return 501 until
// the store exposes commit-anchored directory listing.
func handleCommitAnchoredTopicNode(_ hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		if m.Get(repoName) == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}
		hal.WriteProblem(w, http.StatusNotImplemented, "Not Implemented",
			"commit-anchored topic browsing is not yet supported", r.URL.Path)
	}
}
