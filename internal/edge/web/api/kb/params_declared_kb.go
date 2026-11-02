package kb

import (
	"net/http"
	"net/url"

	"knomit/internal/edge/web/hal"
)

// refuseMalformed answers a query string that does not parse with the same
// 400 refuseUnknown gives it, for a route that refuses unknown names itself
// (the materialize sub-resources, with their own text).
func refuseMalformed(w http.ResponseWriter, r *http.Request) bool {
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "malformed query string: "+err.Error(), r.URL.Path)
		return false
	}
	return true
}

// The other knowledge-base GET routes' declared parameters (user ruling
// 2026-11-02: #718's rule on every KB read route), exactly as openapi.yaml
// lists them per route; TestKBRouteParams_MatchOpenAPI compares the two.
var (
	// GET /repos/{repo}/branches/{branch}/topics and /topics/{path} (with its
	// /facts and /stats sub-resources)
	topicsParams = declare()
	// GET /repos/{repo}/branches/{branch}/changes
	changesParams = declare("prefix", "since", "cursor", "limit")
	// GET /repos/{repo}/branches/{branch}/commits
	commitsParams = declare("limit", "after", "from", "before")
	// GET /repos/{repo}/branches/{branch}/commits/{sha}
	commitDetailParams = declare()
	// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/{path} (and its
	// /incoming and /outgoing sub-resources)
	commitFactParams = declare("fallback")
	// GET /repos/{repo}/branches/{branch}/commits/{sha}/topics[/{path}]
	commitTopicsParams = declare()
	// GET /repos/{repo}/branches/{branch}/activity
	activityParams = declare("path")
	// GET /repos/{repo}/branches/{branch}/completions
	completionsParams = declare("category", "prefix")
	// GET /repos/{repo}/branches/{branch}/domains
	domainsParams = declare()
	// GET /repos/{repo}/branches/{branch}/domains/{name}
	domainFactsParams = declare()
	// GET /repos/{repo}/branches/{branch}/motifs
	motifsParams = declare("path", "q", "sort", "limit", "offset")
	// GET /repos/{repo}/branches/{branch}/motifs/{key}
	motifClusterParams = declare("limit")
	// GET /repos/{repo}/branches/{branch}/stats
	statsParams = declare("path", "axis")
	// GET /repos/{repo}/branches/{branch}/events
	eventsParams = declare()
	// GET /repos/{repo}/branches/{branch}/triggers
	triggersParams = declare("log", "run")
	// GET /repos/{repo}/branches/{branch}/facts/{path} (and its /incoming
	// and /outgoing sub-resources); GET /lenses/{lens}/facts/{path} too
	factReadParams = declare()
	// GET /repos/{repo}/branches/{branch}/facts/{path}/commits (the lens
	// route serves the same sub-resource)
	factCommitsParams = declare("limit", "after", "from", "before")
	// GET /lenses/{lens}/completions
	lensCompletionsParams = declare("category", "prefix")
	// GET /lenses/{lens}/motifs
	lensMotifsParams = declare("path", "repo", "q", "sort", "limit", "offset")
	// GET /lenses/{lens}/motifs/{key}
	lensMotifClusterParams = declare("limit")
	// GET /lenses/{lens}/stats
	lensStatsParams = declare("path", "repo", "axis")
	// GET /lenses/{lens}/topics[/{path}]
	lensTopicsParams = declare("repo")
)

// refuseUnknownFactPath applies the declared set of a GET /facts/* request
// (or, commitAnchored, /commits/{sha}/facts/*) by what the path addresses:
// the materialize sub-resources refuse unknown names themselves (only a
// malformed query is refused here), /commits is factCommitsParams, and the
// fact read with its /incoming and /outgoing sub-resources is factReadParams
// (commitFactParams at a commit, which also takes fallback).
func refuseUnknownFactPath(w http.ResponseWriter, r *http.Request, path string, commitAnchored bool) bool {
	if _, ok := splitMaterializePages(path); ok {
		return refuseMalformed(w, r)
	}
	if _, ok := splitMaterialize(path); ok {
		return refuseMalformed(w, r)
	}
	if commitAnchored {
		return commitFactParams.refuseUnknown(w, r)
	}
	if _, sub := splitFactSubResource(path); sub == "commits" {
		return factCommitsParams.refuseUnknown(w, r)
	}
	return factReadParams.refuseUnknown(w, r)
}
