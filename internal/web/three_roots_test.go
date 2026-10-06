package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// F25 — three roots, the REST doors, against a real store.

var restSystemAreas = []string{"guidance", "skills", "recipes", "triggers", "runs"}

type threeRootsREST struct {
	m  *repos.Manager
	ri *repos.RepoInstance
	r  http.Handler
	b  string
}

func newThreeRootsREST(t *testing.T) threeRootsREST {
	t.Helper()
	m := contextManager(t, "alpha", "beta")
	ri := m.Get("alpha")
	// Written by git (the store directly), as a person's push would: one file
	// in each .knomit/ area, a hidden draft, and a real fact.
	for _, a := range restSystemAreas {
		seedVerdict(t, ri, ".knomit/"+a+"/x.md", "")
	}
	seedVerdict(t, ri, "kb/.drafts/x.md", "")
	seedVerdict(t, ri, "kb/verdicts/real.md", "")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	return threeRootsREST{m: m, ri: ri, r: r, b: urlBranch(ri.AgentBranch())}
}

func (h threeRootsREST) do(t *testing.T, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, fromLoopback(req))
	return rec
}

func (h threeRootsREST) tip(t *testing.T) string {
	t.Helper()
	var head string
	require.NoError(t, h.ri.WithRead(func(svc *store.Service) {
		var err error
		head, err = svc.Branches().HeadCommit(context.Background(), h.ri.AgentBranch())
		require.NoError(t, err)
	}))
	return head
}

func (h threeRootsREST) content(t *testing.T, path string) (string, bool) {
	t.Helper()
	var out string
	var ok bool
	require.NoError(t, h.ri.WithRead(func(svc *store.Service) {
		f, err := svc.Facts().ReadFact(context.Background(), h.ri.AgentBranch(), path, nil)
		if err == nil {
			out, ok = f.Content, true
		}
	}))
	return out, ok
}

func putBody(t *testing.T, ctxLine string) string {
	t.Helper()
	content, err := json.Marshal(map[string]string{"content": "---\ntype: observation\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: []\n" + ctxLine + "---\n# P\n\nINJECTED\n"})
	require.NoError(t, err)
	return string(content)
}

// T3 (closed, REST): PUT and DELETE refuse every .knomit/ area and every
// other dot path with 400, and the tip does not move.
func TestThreeRoots_REST_WritesRefuseDotPaths(t *testing.T) {
	h := newThreeRootsREST(t)
	paths := []string{"kb/.drafts/x.md", ".github/x.md"}
	for _, a := range restSystemAreas {
		paths = append(paths, ".knomit/"+a+"/x.md", ".knomit/"+a+"/new.md")
	}
	for _, p := range paths {
		before := h.tip(t)
		rec := h.do(t, http.MethodPut, "/repos/alpha/branches/"+h.b+"/facts/"+p, putBody(t, ""))
		require.Equalf(t, http.StatusBadRequest, rec.Code, "PUT %s: %s", p, rec.Body.String())
		require.Containsf(t, rec.Body.String(), "closed to the fact endpoints", "PUT %s", p)
		rec = h.do(t, http.MethodDelete, "/repos/alpha/branches/"+h.b+"/facts/"+p, "")
		require.Equalf(t, http.StatusBadRequest, rec.Code, "DELETE %s: %s", p, rec.Body.String())
		require.Equalf(t, before, h.tip(t), "tip moved for %s", p)
	}
	for _, a := range restSystemAreas {
		c, ok := h.content(t, ".knomit/"+a+"/x.md")
		require.True(t, ok)
		require.NotContains(t, c, "INJECTED")
	}
}

// T4 (reads, REST): GET on the branch, at a commit, through a lens, and every
// sub-resource refuses a dot path with 400 — the files exist, so a 404 would
// mean the guard was skipped. The kb fact reads fine on every route.
func TestThreeRoots_REST_ReadsRefuseDotPaths(t *testing.T) {
	h := newThreeRootsREST(t)
	head := h.tip(t)
	closed := []string{"kb/.drafts/x.md", ".knomit/skills/x.md", ".knomit/guidance/x.md"}
	for _, p := range closed {
		for _, url := range []string{
			"/repos/alpha/branches/" + h.b + "/facts/" + p,
			"/repos/alpha/branches/" + h.b + "/facts/" + p + "/commits",
			"/repos/alpha/branches/" + h.b + "/facts/" + p + "/incoming",
			"/repos/alpha/branches/" + h.b + "/facts/" + p + "/outgoing",
			"/repos/alpha/branches/" + h.b + "/commits/" + head + "/facts/" + p,
			"/repos/alpha/branches/" + h.b + "/commits/" + head + "/facts/" + p + "/incoming",
			"/lenses/eng/facts/" + p,
			"/lenses/eng/facts/" + p + "/commits",
			// Percent-encoded dot: not a way around the guard.
			"/repos/alpha/branches/" + h.b + "/facts/" + strings.Replace(p, ".", "%2E", 1),
		} {
			rec := h.do(t, http.MethodGet, url, "")
			require.Equalf(t, http.StatusBadRequest, rec.Code, "GET %s: %s", url, rec.Body.String())
			require.Containsf(t, rec.Body.String(), "closed to the fact endpoints", "GET %s", url)
		}
	}
	for _, url := range []string{
		"/repos/alpha/branches/" + h.b + "/facts/kb/verdicts/real.md",
		"/repos/alpha/branches/" + h.b + "/commits/" + head + "/facts/kb/verdicts/real.md",
		"/lenses/eng/facts/kb/verdicts/real.md",
	} {
		rec := h.do(t, http.MethodGet, url, "")
		require.Equalf(t, http.StatusOK, rec.Code, "GET %s: %s", url, rec.Body.String())
	}
}

// T5 (open, REST): an artifact round-trips through PUT, GET and DELETE — at
// the repo root, in lowercase, whatever case the caller used. A malformed
// artifact path is refused in any case (R1: the store lowercases the whole
// path, so the check must not be case-sensitive), and a context on an
// artifact is refused.
func TestThreeRoots_REST_ArtifactRoundTrip(t *testing.T) {
	h := newThreeRootsREST(t)
	base := "/repos/alpha/branches/" + h.b + "/facts/"

	rec := h.do(t, http.MethodPut, base+"artifacts/runs/y.md", putBody(t, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = h.do(t, http.MethodGet, base+"artifacts/runs/y.md", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "INJECTED")
	rec = h.do(t, http.MethodDelete, base+"artifacts/runs/y.md", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, ok := h.content(t, "artifacts/runs/y.md")
	require.False(t, ok)

	// Mixed case: lands lowercased at the repo root, reads back, deletes.
	rec = h.do(t, http.MethodPut, base+"Artifacts/Runs/Z.md", putBody(t, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, ok = h.content(t, "artifacts/runs/z.md")
	require.True(t, ok, "stored lowercased at the repo root")
	_, ok = h.content(t, "kb/artifacts/runs/z.md")
	require.False(t, ok)
	rec = h.do(t, http.MethodGet, base+"artifacts/runs/z.md", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = h.do(t, http.MethodDelete, base+"ARTIFACTS/RUNS/Z.md", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, ok = h.content(t, "artifacts/runs/z.md")
	require.False(t, ok)

	// Malformed, in any case: 400, nothing written.
	for _, p := range []string{"artifacts/x.md", "Artifacts/x.md", "ARTIFACTS/x.md", "artifacts//x.md", "Artifacts/a/.b/c.md", "artifacts"} {
		before := h.tip(t)
		rec = h.do(t, http.MethodPut, base+p, putBody(t, ""))
		require.Equalf(t, http.StatusBadRequest, rec.Code, "PUT %s: %s", p, rec.Body.String())
		rec = h.do(t, http.MethodDelete, base+p, "")
		require.Equalf(t, http.StatusBadRequest, rec.Code, "DELETE %s: %s", p, rec.Body.String())
		require.Equalf(t, before, h.tip(t), "tip moved for %s", p)
	}

	// A context on an artifact: refused, no ontology applies there.
	rec = h.do(t, http.MethodPut, base+"artifacts/runs/c.md", putBody(t, "context: {task: t-1}\n"))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "context is not allowed here")
}

// T9 (the F23 case): on a repo with no origin, every REST door tries to put
// .knomit/guidance/x.md on the agent branch; after a reconcile round the
// consensus tip does not carry it. (The MCP doors and resolutions are driven
// the same way in internal/mcp and internal/resolutions.)
func TestThreeRoots_REST_GuidanceNeverReachesConsensus(t *testing.T) {
	m := contextManager(t, "alpha")
	ri := m.Get("alpha")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	b := urlBranch(ri.AgentBranch())
	h := threeRootsREST{m: m, ri: ri, r: r, b: b}

	rec := h.do(t, http.MethodPut, "/repos/alpha/branches/"+b+"/facts/.knomit/guidance/x.md", putBody(t, ""))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	// POST derives the path itself (from topic/category, under kb/); whatever
	// it answers, it must not have placed anything under .knomit/ — checked
	// on the consensus tip below.
	rec = h.do(t, http.MethodPost, "/repos/alpha/branches/"+b+"/facts",
		`{"title":"X","body":"INJECTED","topic":".knomit","category":"guidance","type":"observation","confidence":0.5,"sources":1}`)
	if rec.Code == http.StatusCreated {
		require.NotContains(t, rec.Body.String(), `"path":".knomit/`, "POST must never allocate under .knomit/")
	}
	// Something legitimate, so the reconcile round has a tip to advance to.
	seedVerdict(t, ri, "kb/verdicts/after.md", "")

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		rem, err := svc.Remote().GetRemote("origin")
		require.NoError(t, err)
		require.Nil(t, rem, "the case under test is a repo with no origin")
		_, err = svc.AdvanceLocalUpstream(context.Background(), ri.AgentBranch(), svc.UpstreamBranch())
		require.NoError(t, err)
		up := svc.UpstreamBranch()
		ok, err := svc.Facts().FactExists(context.Background(), up, "kb/verdicts/after.md")
		require.NoError(t, err)
		require.True(t, ok, "the reconcile round ran: the consensus tip carries the legitimate write")
		ok, err = svc.Facts().FactExists(context.Background(), up, ".knomit/guidance/x.md")
		require.NoError(t, err)
		require.False(t, ok, "no fact door put .knomit/guidance/x.md on the consensus tip")
	}))
}
