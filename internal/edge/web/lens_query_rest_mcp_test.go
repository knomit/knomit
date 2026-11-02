package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// A lens reads the same on both surfaces (user ruling 2026-11-02, "align
// them"): knomit_query on the lens's MCP endpoint and the REST lens list
// (/lenses/{lens}/facts, /lenses/{lens}/search) return the same rows, in the
// same order, for the same filters — recency deepening and repo= narrowing
// included — and refuse the same narrowing with the same text.

const (
	engFacts  = "/api/v1/lenses/eng/facts"
	engSearch = "/api/v1/lenses/eng/search"
)

// query runs knomit_query on s's endpoint.
func (s mcpSession) query(args string) (string, bool) {
	s.t.Helper()
	return callToolAt(s.t, s.h, s.endpoint, s.sid, "knomit_query", args)
}

// mcpQueryFiles is the ordered list of `file`s of a knomit_query response.
func mcpQueryFiles(t *testing.T, raw string) []string {
	t.Helper()
	var r struct {
		Facts []struct {
			File string `json:"file"`
		} `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &r), raw)
	out := make([]string, 0, len(r.Facts))
	for _, f := range r.Facts {
		out = append(out, f.File)
	}
	return out
}

// restListPaths is the ordered list of paths of a REST lens /facts or
// /search response (rows under "facts" or "results").
func restListPaths(t *testing.T, h http.Handler, u string) []string {
	t.Helper()
	code, body := restGet(t, h, u)
	require.Equal(t, http.StatusOK, code, body)
	var r struct {
		Facts []struct {
			Path string `json:"path"`
		} `json:"facts"`
		Results []struct {
			Path string `json:"path"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &r), body)
	out := []string{}
	for _, f := range append(r.Facts, r.Results...) {
		out = append(out, f.Path)
	}
	return out
}

// restProblemDetail GETs u, requires status want, and returns the problem
// document's detail.
func restProblemDetail(t *testing.T, h http.Handler, u string, want int) string {
	t.Helper()
	code, body := restGet(t, h, u)
	require.Equal(t, want, code, body)
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	return p.Detail
}

// sort=recent goes as deep on MCP as on REST. Lens eng = alpha (write) + beta.
// beta holds copies of alpha's three facts, committed AFTER its own unique u1,
// so beta's newest three rows are copies that lose to alpha (write-first). At
// a per-mount depth of 3, the merge holds only alpha's three rows, and u1 —
// newer than every one of them — sits at beta's depth 4. REST /facts?limit=3
// deepens (beta is truncated and no merged row is at or above its horizon)
// and lists u1 first; knomit_query sort=recent max_results=3 must list the
// same three rows.
//
// Sabotage: put MCP queryRecent back on one FusedRecent round at max_results
// (MCP lists alpha's p1, p2, p3 and no u1).
func TestLensQuery_RecentDepthIsRESTs(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	alpha, beta := e.m.Get("alpha"), e.m.Get("beta")
	writeFactsInOneCommit(t, alpha,
		factSpec{path: "kb/p/p1.md", title: "alpha p1"},
		factSpec{path: "kb/p/p2.md", title: "alpha p2"},
		factSpec{path: "kb/p/p3.md", title: "alpha p3"},
	)
	awaitNextSecond()
	writeFactsInOneCommit(t, beta, factSpec{path: "kb/u/u1.md", title: "beta u1"})
	awaitNextSecond()
	writeFactsInOneCommit(t, beta,
		factSpec{path: "kb/p/p1.md", title: "beta p1"},
		factSpec{path: "kb/p/p2.md", title: "beta p2"},
		factSpec{path: "kb/p/p3.md", title: "beta p3"},
	)
	h := e.as(t, "reader")
	u1 := "kb://" + fact.ID12(beta.ID()) + "/kb/u/u1.md"

	rest := restListPaths(t, h, engFacts+"?limit=3")
	require.Equal(t, []string{u1, "kb/p/p1.md", "kb/p/p2.md"}, rest, "REST deepens past beta's copies to u1")

	mcp := newMCPSession(t, h, engMCP)
	text, isErr := mcp.query(`{"sort":"recent","max_results":3,"limit":3}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text), "knomit_query sort=recent lists REST's rows")

	// The default (max_results 500, a deeper start than any mount holds) agrees
	// with REST's same-sized page too.
	text, isErr = mcp.query(`{"sort":"recent","limit":4}`)
	require.False(t, isErr, text)
	require.Equal(t, restListPaths(t, h, engFacts+"?limit=4"), mcpQueryFiles(t, text))
}

// repo= narrows knomit_query exactly as it narrows the REST lens list, on the
// relevance read (/search) and the recency read (/facts), and an unknown name
// is the same text on both surfaces (REST: 422 "Unknown repo" detail; MCP: the
// tool error).
//
// Sabotage: drop Repos from MCP planFused (beta-only lists gain alpha's and
// gamma's rows; the unknown name is no error).
func TestLensQuery_RepoNarrowingIsRESTs(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta", "gamma")
	seedThreeMembers(t, e.m)
	h := e.as(t, "reader")
	mcp := newMCPSession(t, h, engMCP)
	beta12 := fact.ID12(e.m.Get("beta").ID())
	gamma12 := fact.ID12(e.m.Get("gamma").ID())

	// relevance
	rest := restListPaths(t, h, engSearch+"?q=x&repo=beta&repo=gamma")
	require.ElementsMatch(t, []string{
		"kb://" + beta12 + "/kb/b/b0.md", "kb://" + beta12 + "/kb/b/b1.md", "kb://" + beta12 + "/kb/b/b2.md",
		"kb://" + gamma12 + "/kb/g/g0.md", "kb://" + gamma12 + "/kb/g/g1.md",
	}, rest, "REST narrows to beta and gamma")
	text, isErr := mcp.query(`{"text":"x","repo":["beta","gamma"]}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))

	// recency, a single name as a string
	rest = restListPaths(t, h, engFacts+"?repo=beta")
	require.Len(t, rest, 3)
	text, isErr = mcp.query(`{"sort":"recent","repo":"beta"}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))

	// the write repo is a member too
	rest = restListPaths(t, h, engFacts+"?repo=alpha")
	require.ElementsMatch(t, []string{"kb/a/a0.md", "kb/a/a1.md", "kb/a/a2.md"}, rest)
	text, isErr = mcp.query(`{"sort":"recent","repo":["alpha"]}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))

	// an unknown name: one text on both surfaces, on both reads
	want := `no mount named "nope" in lens "eng"`
	require.Equal(t, want, restProblemDetail(t, h, engSearch+"?q=x&repo=nope", http.StatusUnprocessableEntity))
	require.Equal(t, want, restProblemDetail(t, h, engFacts+"?repo=nope", http.StatusUnprocessableEntity))
	for _, args := range []string{`{"text":"x","repo":["beta","nope"]}`, `{"sort":"recent","repo":"nope"}`} {
		text, isErr = mcp.query(args)
		require.True(t, isErr, "%s: %s", args, text)
		require.Equal(t, want, text, args)
	}
}

// A one-member lens is still a lens: on both surfaces it accepts its one
// member's name (narrowing nothing) and refuses any other with the lens
// list's text. A binding that is not a lens refuses any repo on MCP with the
// FactSet's ruled text (S4); REST has no repo= on a repository's routes.
//
// Sabotage: test IsLens instead of FromLens in MCP planFused (the one-member
// lens refuses its own member); drop the not-a-lens check (alpha's repo
// endpoint answers repo=alpha with its rows, and repo=beta with the
// misleading "no mount named \"beta\" in lens \"alpha\"").
func TestLensQuery_RepoOnAOneMemberLensAndOnARepo(t *testing.T) {
	e := newLensRESTEnv(t, "alpha")
	writeFactsInOneCommit(t, e.m.Get("alpha"),
		factSpec{path: "kb/a/1.md", title: "alpha one"},
		factSpec{path: "kb/a/2.md", title: "alpha two"},
	)
	h := e.as(t, "reader")
	mcp := newMCPSession(t, h, engMCP)

	rest := restListPaths(t, h, engFacts+"?repo=alpha")
	require.Equal(t, []string{"kb/a/1.md", "kb/a/2.md"}, rest)
	text, isErr := mcp.query(`{"sort":"recent","repo":"alpha"}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))
	text, isErr = mcp.query(`{"path":"kb/","repo":"alpha"}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))

	want := `no mount named "beta" in lens "eng"`
	require.Equal(t, want, restProblemDetail(t, h, engFacts+"?repo=beta", http.StatusUnprocessableEntity))
	text, isErr = mcp.query(`{"sort":"recent","repo":"beta"}`)
	require.True(t, isErr, text)
	require.Equal(t, want, text)

	// Not a lens: alpha's own endpoint.
	repoEP := newMCPSession(t, h, "/api/v1/repos/alpha/branches/"+urlBranch(e.m.Get("alpha").AgentBranch())+"/mcp")
	for _, args := range []string{`{"sort":"recent","repo":"alpha"}`, `{"path":"kb/","repo":["beta"]}`} {
		text, isErr = repoEP.query(args)
		require.True(t, isErr, "%s: %s", args, text)
		require.Equal(t, "repo is only accepted on a lens", text, args)
	}
	// Positive control: the same endpoint answers without repo.
	text, isErr = repoEP.query(`{"sort":"recent"}`)
	require.False(t, isErr, text)
	require.Equal(t, rest, mcpQueryFiles(t, text))
}
