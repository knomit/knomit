package kb

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"knomit/internal/edge/web/hal"
)

// declaredParams is the set of query parameters ONE list route declares: the
// route answers 400 to any other (user ruling 2026-11-02: a parameter a list
// route does not declare is refused, never silently ignored). Each set is the
// route's OpenAPI parameter list, aliases and paging included;
// TestListRouteParams_MatchOpenAPI compares the two, so a parameter added to
// one and not the other fails the build.
type declaredParams struct {
	names    map[string]bool
	prefixes []string // a family such as context.<key>
}

// declare builds a declaredParams. A name ending in ".<key>" (OpenAPI's
// spelling of a family, e.g. "context.<key>") declares every parameter that
// starts with the part before "<key>".
func declare(names ...string) declaredParams {
	d := declaredParams{names: map[string]bool{}}
	for _, n := range names {
		if p, ok := strings.CutSuffix(n, "<key>"); ok {
			d.prefixes = append(d.prefixes, p)
			continue
		}
		d.names[n] = true
	}
	return d
}

func (d declaredParams) has(k string) bool {
	if d.names[k] {
		return true
	}
	for _, p := range d.prefixes {
		if strings.HasPrefix(k, p) && len(k) > len(p) {
			return true
		}
	}
	return false
}

// refuseUnknown answers 400 "Invalid parameter" for the first (in name order)
// query parameter d does not declare, with the FactSet routes' own detail
// text (factsetRequest), and reports whether the request may proceed. A query
// string that does not parse (a bad %-escape, a `;` separator) is a 400 too:
// r.URL.Query() drops such a pair without a word, which would let it through
// as if it had never been sent.
func (d declaredParams) refuseUnknown(w http.ResponseWriter, r *http.Request) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "malformed query string: "+err.Error(), r.URL.Path)
		return false
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !d.has(k) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "unknown parameter "+strconv.Quote(k), r.URL.Path)
			return false
		}
	}
	return true
}

// The four list routes' declared parameters, exactly as openapi.yaml lists
// them per route.
var (
	// GET /repos/{repo}/branches/{branch}/search
	repoSearchParams = declare(
		"q", "entities", "domain", "domain_exact", "path", "min_confidence", "min_similarity", "limit",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>", "since_fork",
	)
	// GET /repos/{repo}/branches/{branch}/facts
	repoFactsParams = declare(
		"limit", "offset", "path", "topic", "q", "domain", "entity", "entities",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>", "min_confidence", "since_fork", "sort",
	)
	// GET /lenses/{lens}/search
	lensSearchParams = declare(
		"q", "limit", "path", "repo", "entity", "entities", "domain", "domain_exact", "applies_to",
		"type", "exclude_type", "kind", "exclude_kind", "origin", "ep", "min_confidence", "min_similarity",
		"motifs", "motif_match", "expired", "expires_before", "expires_after", "context.<key>",
	)
	// GET /lenses/{lens}/facts
	lensFactsParams = declare(
		"limit", "offset", "query", "q", "path", "topic", "repo", "domain", "domain_exact", "applies_to",
		"entity", "entities", "type", "exclude_type", "kind", "exclude_kind", "origin", "ep",
		"min_confidence", "min_similarity", "motifs", "motif_match",
		"expired", "expires_before", "expires_after", "context.<key>",
	)
)
