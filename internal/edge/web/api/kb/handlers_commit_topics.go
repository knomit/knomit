package kb

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
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
		if !commitTopicsParams.RefuseUnknown(w, r) {
			return
		}
		// An unknown commit is 404 commit_not_in_repo even here: what the
		// request names does not exist, whatever the route can serve.
		if !refuseUnknownCommit(w, r, repos.RepoFromContext(r.Context()), chi.URLParam(r, "sha")) {
			return
		}
		hal.WriteProblem(w, http.StatusNotImplemented, "Not Implemented",
			"commit-anchored topic browsing is not yet supported", r.URL.Path)
	}
}
