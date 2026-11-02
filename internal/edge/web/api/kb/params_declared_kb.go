package kb

import (
	"net/http"

	"knomit/internal/corpus/federate"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/httpx"
)

// The other knowledge-base GET routes' declared parameters (user ruling
// 2026-11-02: #718's rule on every KB read route), exactly as openapi.yaml
// lists them per route; TestKBRouteParams_MatchOpenAPI compares the two.
var (
	// GET /repos/{repo}/branches/{branch}/topics and /topics/{path} (with its
	// /facts and /stats sub-resources)
	topicsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/changes
	changesParams = httpx.DeclareParams("prefix", "since", "cursor", "limit")
	// GET /repos/{repo}/branches/{branch}/commits
	commitsParams = httpx.DeclareParams("limit", "after", "from", "before")
	// GET /repos/{repo}/branches/{branch}/commits/{sha}
	commitDetailParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/commits/{sha}/facts/{path} (and its
	// /incoming and /outgoing sub-resources)
	commitFactParams = httpx.DeclareParams("fallback")
	// GET /repos/{repo}/branches/{branch}/commits/{sha}/topics[/{path}]
	commitTopicsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/activity
	activityParams = httpx.DeclareParams("path")
	// GET /repos/{repo}/branches/{branch}/completions
	completionsParams = httpx.DeclareParams("category", "prefix")
	// GET /repos/{repo}/branches/{branch}/domains
	domainsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/domains/{name}
	domainFactsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/motifs
	motifsParams = httpx.DeclareParams("path", "q", "sort", "limit", "offset")
	// GET /repos/{repo}/branches/{branch}/motifs/{key}
	motifClusterParams = httpx.DeclareParams("limit")
	// GET /repos/{repo}/branches/{branch}/stats
	statsParams = httpx.DeclareParams("path", "axis")
	// GET /repos/{repo}/branches/{branch}/events
	eventsParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/triggers
	triggersParams = httpx.DeclareParams("log", "run")
	// GET /repos/{repo}/branches/{branch}/facts/{path} (and its /incoming
	// and /outgoing sub-resources); GET /lenses/{lens}/facts/{path} too
	factReadParams = httpx.DeclareParams()
	// GET /repos/{repo}/branches/{branch}/facts/{path}/commits (the lens
	// route serves the same sub-resource)
	factCommitsParams = httpx.DeclareParams("limit", "after", "from", "before")
	// GET /lenses/{lens}/completions
	lensCompletionsParams = httpx.DeclareParams("category", "prefix")
	// GET /lenses/{lens}/motifs
	lensMotifsParams = httpx.DeclareParams("path", "repo", "q", "sort", "limit", "offset")
	// GET /lenses/{lens}/motifs/{key}
	lensMotifClusterParams = httpx.DeclareParams("limit")
	// GET /lenses/{lens}/stats
	lensStatsParams = httpx.DeclareParams("path", "repo", "axis")
	// GET /lenses/{lens}/topics[/{path}]
	lensTopicsParams = httpx.DeclareParams("repo")
)

// refuseUnknownFactPath applies the declared set of a GET /facts/* request
// (or, commitAnchored, /commits/{sha}/facts/*) by what the path addresses:
// the materialize sub-resources refuse unknown names themselves (only a
// malformed query is refused here), /commits is factCommitsParams, and the
// fact read with its /incoming and /outgoing sub-resources is factReadParams
// (commitFactParams at a commit, which also takes fallback).
func refuseUnknownFactPath(w http.ResponseWriter, r *http.Request, path string, commitAnchored bool) bool {
	// A raw system file (.knomit/…, bare or kb://-qualified on the lens) is
	// read as a file, never as a sub-resource, whatever its last segment is:
	// `.knomit/x/commits` is a file named commits. Decide that FIRST, exactly
	// as the routes do (serveSystemFileRead runs before any dispatch).
	if isSystemFileRequest(path) {
		return factReadParams.RefuseUnknown(w, r)
	}
	if _, ok := splitMaterializePages(path); ok {
		return httpx.RefuseMalformed(w, r)
	}
	if _, ok := splitMaterialize(path); ok {
		return httpx.RefuseMalformed(w, r)
	}
	if commitAnchored {
		return commitFactParams.RefuseUnknown(w, r)
	}
	if _, sub := splitFactSubResource(path); sub == "commits" {
		return factCommitsParams.RefuseUnknown(w, r)
	}
	return factReadParams.RefuseUnknown(w, r)
}

// isSystemFileRequest reports whether a fact-route path names a raw system
// file: the test serveSystemFileRead and serveLensSystemFileRead apply (a
// kb://<id12>/ qualifier, which only the lens route can carry, is stripped).
func isSystemFileRequest(path string) bool {
	if _, rel, _, err := federate.ParseQualifiedPath(path); err == nil && fact.IsSystemFilePath(rel) {
		return true
	}
	return fact.IsSystemFilePath(path)
}
