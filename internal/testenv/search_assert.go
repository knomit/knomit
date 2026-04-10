package testenv

import (
	"context"
	"testing"

	"knomit/internal/store"
)

// SearchAssert chains assertions over a search query result. Returned by
// BranchHandle.Search so the DSL reads like:
//
//	agent.Search("quantum entanglement").
//	    MustRankFirst("kb/entanglement.md").
//	    MustNotReturn("kb/classical.md")
//
// The query is executed immediately (the DSL does not defer). Results are
// captured as a slice of store.SearchResult — raw access via Results() for
// the rare test that needs to inspect scores or counts directly.
type SearchAssert struct {
	t       *testing.T
	branch  *BranchHandle
	query   string
	results []store.SearchResult
}

// Search runs a text query against the branch's search index via the
// production Search API and returns a SearchAssert over the results. The
// query goes through the real vector-search code path using whatever
// embedder the Storyboard has configured (DeterministicEmbedder by default).
//
// Uses the default SearchQuery with Text set and Limit 50. Tests that need
// more control can drop to the production API via b.repo.Instance().
func (b *BranchHandle) Search(query string) *SearchAssert {
	t := b.repo.sb.t
	t.Helper()
	var results []store.SearchResult
	var err error
	b.repo.ri.WithRead(func(svc *store.Service) {
		results, err = svc.Search().Search(context.Background(), b.name, store.SearchQuery{
			Text:  query,
			Limit: 50,
		})
	})
	if err != nil {
		t.Fatalf("Search(%q on %s): %v", query, b.name, err)
	}
	return &SearchAssert{t: t, branch: b, query: query, results: results}
}

// Results returns the raw slice of store.SearchResult. Use this only when
// the MustReturn / MustNotReturn / MustRankFirst / MustBeEmpty / MustHaveLen
// helpers don't fit.
func (a *SearchAssert) Results() []store.SearchResult { return a.results }

// MustReturn asserts that every path in paths appears somewhere in the
// result set. Order and score are not checked.
func (a *SearchAssert) MustReturn(paths ...string) *SearchAssert {
	a.t.Helper()
	have := make(map[string]bool, len(a.results))
	for _, r := range a.results {
		have[r.Path] = true
	}
	for _, p := range paths {
		if !have[p] {
			a.t.Fatalf("search(%q on %s): missing %q in results %v", a.query, a.branch.name, p, resultPaths(a.results))
		}
	}
	return a
}

// MustNotReturn asserts that none of the given paths appear in the result
// set.
func (a *SearchAssert) MustNotReturn(paths ...string) *SearchAssert {
	a.t.Helper()
	have := make(map[string]bool, len(a.results))
	for _, r := range a.results {
		have[r.Path] = true
	}
	for _, p := range paths {
		if have[p] {
			a.t.Fatalf("search(%q on %s): unexpectedly returned %q (results: %v)", a.query, a.branch.name, p, resultPaths(a.results))
		}
	}
	return a
}

// MustRankFirst asserts the top-scoring result is at the given path.
// Fails if the result set is empty or the top path doesn't match.
func (a *SearchAssert) MustRankFirst(path string) *SearchAssert {
	a.t.Helper()
	if len(a.results) == 0 {
		a.t.Fatalf("search(%q on %s): empty result, cannot assert rank-first", a.query, a.branch.name)
	}
	if a.results[0].Path != path {
		a.t.Fatalf("search(%q on %s): top hit %q, want %q (results: %v)", a.query, a.branch.name, a.results[0].Path, path, resultPaths(a.results))
	}
	return a
}

// MustBeEmpty asserts the search returned zero results.
func (a *SearchAssert) MustBeEmpty() *SearchAssert {
	a.t.Helper()
	if len(a.results) != 0 {
		a.t.Fatalf("search(%q on %s): expected empty, got %v", a.query, a.branch.name, resultPaths(a.results))
	}
	return a
}

// MustHaveLen asserts the result set has exactly n entries.
func (a *SearchAssert) MustHaveLen(n int) *SearchAssert {
	a.t.Helper()
	if len(a.results) != n {
		a.t.Fatalf("search(%q on %s): got %d results, want %d (%v)", a.query, a.branch.name, len(a.results), n, resultPaths(a.results))
	}
	return a
}

func resultPaths(results []store.SearchResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.Path
	}
	return out
}
