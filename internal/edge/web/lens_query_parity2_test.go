package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// Follow-up to the REST == MCP lens parity (user rulings 2026-11-02): one
// per-mount relevance depth on both surfaces, no filter required on MCP,
// entity= honoured on REST /search, and one filter set on knomit_query and
// the REST lens reads.

// seedDeepWriteCopy writes, into alpha (the write mount), 21 facts that rank
// above its own copy of kb/shared/x.md for any query, and into beta a copy of
// kb/shared/x.md that ranks first. At a per-mount depth of 20, alpha's copy is
// not fetched, so beta's copy survives the write-first dedupe and ranks near
// the top; at REST's depth (federate.MaxSearchCandidates) alpha's copy is
// fetched, wins the path, and sits 22nd: outside a 20-row list.
func seedDeepWriteCopy(t *testing.T, e lensRESTEnv) string {
	t.Helper()
	specs := make([]factSpec, 0, 22)
	for i := 0; i < 21; i++ {
		specs = append(specs, factSpec{path: fmt.Sprintf("kb/a/%02d.md", i), title: fmt.Sprintf("alpha %02d", i), cos: 0.95 - 0.01*float64(i)})
	}
	specs = append(specs, factSpec{path: "kb/shared/x.md", title: "alpha x", cos: 0.6})
	writeFactsInOneCommit(t, e.m.Get("alpha"), specs...)
	writeFactsInOneCommit(t, e.m.Get("beta"), factSpec{path: "kb/shared/x.md", title: "beta x", cos: 0.99})
	return "kb://" + fact.ID12(e.m.Get("beta").ID()) + "/kb/shared/x.md"
}

// knomit_query asks every lens mount for REST's depth, so a small max_results
// caps only the fused list: relevance with max_results=20 equals REST
// /search?limit=20, and text + sort=recent with max_results=20 equals REST
// /facts?q=x&limit=20, row for row, neither showing beta's shadowed copy.
//
// Sabotage: ask each mount for max_results again in queryFirstCall (relevance
// shows beta's x); start FusedRecentDeep at max_results for text in
// queryRecent (text + sort=recent shows beta's x).
func TestLensQuery_RelevanceDepthIsRESTs(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	betaX := seedDeepWriteCopy(t, e)
	h := e.as(t, "reader")
	mcp := newMCPSession(t, h, engMCP)

	for _, tc := range []struct{ name, restURL, mcpArgs string }{
		{"relevance", engSearch + "?q=x&limit=20", `{"text":"x","max_results":20}`},
		{"text + sort=recent", engFacts + "?q=x&limit=20", `{"text":"x","sort":"recent","max_results":20,"limit":20}`},
	} {
		rest := restListPaths(t, h, tc.restURL)
		require.Len(t, rest, 20, tc.name)
		require.Equal(t, "kb/a/00.md", rest[0], tc.name)
		require.NotContains(t, rest, betaX, "%s: REST's depth fetches alpha's copy, which wins the path", tc.name)
		text, isErr := mcp.query(tc.mcpArgs)
		require.False(t, isErr, text)
		require.Equal(t, rest, mcpQueryFiles(t, text), tc.name)
	}
}

// A filterless knomit_query is allowed (no filter required), and its first
// page is bounded by the defaults: exactly defaultPageSize (20) rows of
// ~400-rune snippets marked body_truncated, a cursor for the rest. The rows
// are REST /search's (no filter, limit=20). include_body pages are 3 rows.
//
// Sabotage: put the no-filter refusal back in queryFirstCall (an error); or
// make snippet mode return full bodies (rows longer than a snippet).
func TestLensQuery_FilterlessFirstPageIsBounded(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	long := strings.Repeat("lorem ipsum dolor sit amet ", 40) // > 1000 runes
	body := func(f *fact.Fact) { f.Body = "cos=0.9 " + long }
	var specs []factSpec
	for i := 0; i < 15; i++ {
		specs = append(specs, factSpec{path: fmt.Sprintf("kb/a/%02d.md", i), title: fmt.Sprintf("alpha %02d", i), mod: body})
	}
	writeFactsInOneCommit(t, e.m.Get("alpha"), specs...)
	specs = specs[:0]
	for i := 0; i < 15; i++ {
		specs = append(specs, factSpec{path: fmt.Sprintf("kb/b/%02d.md", i), title: fmt.Sprintf("beta %02d", i), mod: body})
	}
	writeFactsInOneCommit(t, e.m.Get("beta"), specs...)
	h := e.as(t, "reader")
	mcp := newMCPSession(t, h, engMCP)

	type row struct {
		File          string `json:"file"`
		Body          string `json:"body"`
		BodyTruncated bool   `json:"body_truncated"`
	}
	var page struct {
		Facts   []row   `json:"facts"`
		Cursor  *string `json:"cursor"`
		HasMore bool    `json:"has_more"`
	}
	text, isErr := mcp.query(`{}`)
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &page), text)
	require.Len(t, page.Facts, 20, "the default snippet page")
	require.True(t, page.HasMore)
	require.NotNil(t, page.Cursor)
	for _, r := range page.Facts {
		require.True(t, r.BodyTruncated, "%s: a snippet, not the body", r.File)
		require.LessOrEqual(t, utf8.RuneCountInString(r.Body), 401, "%s: snippet length", r.File)
		require.Less(t, len(r.Body), len(long), r.File)
	}
	files := make([]string, 0, len(page.Facts))
	for _, r := range page.Facts {
		files = append(files, r.File)
	}
	require.Equal(t, restListPaths(t, h, engSearch+"?limit=20"), files, "REST /search with no filter lists the same rows")

	page.Facts = nil
	text, isErr = mcp.query(`{"include_body":true}`)
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &page), text)
	require.Len(t, page.Facts, 3, "the default include_body page")
	require.False(t, page.Facts[0].BodyTruncated)
	require.Contains(t, page.Facts[0].Body, strings.TrimSpace(long), "the full body")
}

// REST lens /search honours entity= (the canonical name) as /facts does,
// merged with the entities= alias; it is no longer silently dropped.
//
// Sabotage: read only entities= on /search again (entity=E lists all three
// facts).
func TestLensSearch_EntityIsHonoured(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	ent := func(names ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Entities = names } }
	writeFactsInOneCommit(t, e.m.Get("alpha"),
		factSpec{path: "kb/a/e.md", title: "alpha E", mod: ent("E")},
		factSpec{path: "kb/a/f.md", title: "alpha F", mod: ent("F")},
	)
	writeFactsInOneCommit(t, e.m.Get("beta"), factSpec{path: "kb/b/e.md", title: "beta E", mod: ent("E")})
	h := e.as(t, "reader")
	beta12 := fact.ID12(e.m.Get("beta").ID())
	want := []string{"kb/a/e.md", "kb://" + beta12 + "/kb/b/e.md"}

	require.ElementsMatch(t, want, restListPaths(t, h, engSearch+"?q=x&entity=E"))
	require.Equal(t, restListPaths(t, h, engSearch+"?q=x&entities=E"), restListPaths(t, h, engSearch+"?q=x&entity=E"))
	require.ElementsMatch(t, want, restListPaths(t, h, engFacts+"?entity=E"), "and /facts, as before")
	// Both names given: the values merge (all must be present).
	require.Empty(t, restListPaths(t, h, engSearch+"?q=x&entity=E&entities=F"))

	mcp := newMCPSession(t, h, engMCP)
	text, isErr := mcp.query(`{"text":"x","entities":["E"]}`)
	require.False(t, isErr, text)
	require.Equal(t, restListPaths(t, h, engSearch+"?q=x&entity=E"), mcpQueryFiles(t, text))
}

// knomit_query and the REST lens reads take one filter set. The four MCP
// gained (exclude_type, kind, exclude_kind, ep) and the one the REST lens
// reads gained (applies_to) each select exactly the expected rows, and MCP
// equals REST for each, on the relevance read (/search, text) and the
// recency read (/facts, sort=recent).
//
// Sabotage: drop each new field from MCP parseQueryFilters, and applies_to
// from REST /search and from /facts, one at a time (its case fails).
func TestLensQuery_FilterParamsAreRESTs(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	alpha, beta := e.m.Get("alpha"), e.m.Get("beta")
	dom := func(d ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Domain = d } }
	writeFactsInOneCommit(t, alpha,
		factSpec{path: "kb/a/obs.md", title: "alpha obs", cos: 0.9, mod: dom("store/resolver")},
		factSpec{path: "kb/a/pol.md", title: "alpha pol", cos: 0.8, mod: func(f *fact.Fact) {
			f.Kind, f.Type, f.Domain = fact.Pragmatic, fact.Policy, []string{"web"}
		}},
	)
	writeFactsInOneCommit(t, beta, factSpec{path: "kb/b/obs.md", title: "beta obs", cos: 0.85, mod: dom("store")})
	writeFactOp(t, beta, "kb/b/upd.md", "beta upd", 0.7, "update", dom("web"))
	h := e.as(t, "reader")
	mcp := newMCPSession(t, h, engMCP)
	b12 := "kb://" + fact.ID12(beta.ID()) + "/"
	aObs, aPol, bObs, bUpd := "kb/a/obs.md", "kb/a/pol.md", b12+"kb/b/obs.md", b12+"kb/b/upd.md"

	all := restListPaths(t, h, engFacts)
	require.ElementsMatch(t, []string{aObs, aPol, bObs, bUpd}, all, "control: four facts unfiltered")

	for _, tc := range []struct {
		name, rest, mcp string
		want            []string
	}{
		{"exclude_type", "exclude_type=policy", `"exclude_type":["policy"]`, []string{aObs, bObs, bUpd}},
		{"kind", "kind=pragmatic", `"kind":["pragmatic"]`, []string{aPol}},
		{"exclude_kind", "exclude_kind=pragmatic", `"exclude_kind":["pragmatic"]`, []string{aObs, bObs, bUpd}},
		{"ep", "ep=update", `"ep":["update"]`, []string{bUpd}},
		{"applies_to", "applies_to=store/resolver", `"applies_to":["store/resolver"]`, []string{aObs, bObs}},
	} {
		for _, read := range []struct{ name, restURL, mcpArgs string }{
			{"relevance", engSearch + "?q=x&" + tc.rest, `{"text":"x",` + tc.mcp + `}`},
			{"recency", engFacts + "?" + tc.rest, `{"sort":"recent",` + tc.mcp + `}`},
		} {
			rest := restListPaths(t, h, read.restURL)
			require.ElementsMatch(t, tc.want, rest, "%s %s: REST", tc.name, read.name)
			text, isErr := mcp.query(read.mcpArgs)
			require.False(t, isErr, text)
			require.Equal(t, rest, mcpQueryFiles(t, text), "%s %s: MCP == REST", tc.name, read.name)
		}
	}
}
