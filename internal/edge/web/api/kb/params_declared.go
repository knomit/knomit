package kb

import "knomit/internal/edge/web/httpx"

// The declared-parameter mechanism (httpx.DeclaredParams, RefuseUnknown) is
// shared with the management API; see internal/edge/web/httpx/params_declared.go.

// The four list routes' declared parameters, exactly as openapi.yaml lists
// them per route.
var (
	// GET /repos/{repo}/branches/{branch}/search
	repoSearchParams = httpx.DeclareParams(
		"q", "entities", "domain", "domain_exact", "path", "min_confidence", "min_similarity", "limit",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>", "since_fork",
	)
	// GET /repos/{repo}/branches/{branch}/facts
	repoFactsParams = httpx.DeclareParams(
		"limit", "offset", "path", "topic", "q", "domain", "entity", "entities",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>", "min_confidence", "since_fork", "sort",
	)
	// GET /lenses/{lens}/search
	lensSearchParams = httpx.DeclareParams(
		"q", "limit", "path", "repo", "entity", "entities", "domain", "domain_exact", "applies_to",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "min_confidence", "min_similarity",
		"motifs", "motif_match", "expired", "expires_before", "expires_after", "context.<key>",
	)
	// GET /lenses/{lens}/facts
	lensFactsParams = httpx.DeclareParams(
		"limit", "offset", "query", "q", "path", "topic", "repo", "domain", "domain_exact", "applies_to",
		"entity", "entities", "type", "exclude_type", "kind", "exclude_kind", "origin", "ep",
		"min_confidence", "min_similarity", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>",
	)
)
