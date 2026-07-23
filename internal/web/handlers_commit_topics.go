package web

import (
	"net/http"

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
// The repo is resolved (and 404'd) by RepoMiddleware before this runs, so an
// unknown repo still reports "not found" rather than "not implemented".
func handleCommitAnchoredTopicNode() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hal.WriteProblem(w, http.StatusNotImplemented, "Not Implemented",
			"commit-anchored topic browsing is not yet supported", r.URL.Path)
	}
}
