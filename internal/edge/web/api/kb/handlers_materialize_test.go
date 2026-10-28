package kb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// Every fixture here sets the ontology root, as a configured server always
// does (config refuses an empty ontology_root): materialize resolves a fact
// path against it, as knomit_explain does.

// newMatStoreManager is newRealStoreManager with the ontology root set.
func newMatStoreManager(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	for _, name := range names {
		svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
		m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: name, AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
		}))
	}
	return m
}

// newMatLensManager is newTestLensManager with the ontology root set.
func newMatLensManager(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("machine/test"), AgentBranch: "machine/test",
		Machine: repos.Options{Synchronous: true},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	for _, name := range names {
		createRepo(t, m, name)
	}
	return m
}

// writeMatFact writes one observation through the store and returns its
// commit.
func writeMatFact(t *testing.T, ri *repos.RepoInstance, branch, path, title, body string, refs ...string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title, f.Body, f.Type = title, body, fact.Observation
	f.Domain, f.Entities, f.Refs = []string{"testing"}, []string{}, refs
	f.Confidence, f.Sources = 0.7, 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		r, werr := svc.Facts().WriteFact(context.Background(), branch, path, content, "write "+path, "")
		require.NoError(t, werr)
		commit = r.CommitHash
	}))
	return commit
}

func getMaterialize(t *testing.T, h http.Handler, url string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

// T14 (REST half): the route returns exactly RenderAgent of the plan the MCP
// tool pages (its MCP half asserts the pages concatenate to the same
// document), honours its options, follows cross-KB refs into any registered
// repo, and anchors at a commit on the commit-anchored route.
func TestMaterializeRoute_IsTheAgentDocument(t *testing.T) {
	m := newMatStoreManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	const branch = "agent/test"
	beta12 := fact.ID12(beta.ID())

	writeMatFact(t, beta, branch, "kb/b/x.md", "X", "X body.")
	writeMatFact(t, alpha, branch, "kb/a/c.md", "C", "C body.")
	c1 := writeMatFact(t, alpha, branch, "kb/a/b.md", "B", "B first.", "kb/a/c.md")
	writeMatFact(t, alpha, branch, "kb/a/r.md", "R", "R body.", "kb/a/b.md", "kb://"+beta12+"/kb/b/x.md")
	writeMatFact(t, alpha, branch, "kb/a/b.md", "B", "B second.", "kb/a/c.md")
	h := newTestRouter(m)
	base := "/repos/alpha/branches/" + urlBranch(branch)

	code, ctype, body := getMaterialize(t, h, base+"/facts/kb/a/r.md/materialize?depth=1&cross_kb=true")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "text/markdown; charset=utf-8", ctype)
	doc, err := materialize.Plan(context.Background(), materialize.Request{
		Binding: repos.NewBindingOfRepo(alpha, branch), Manager: m, File: "kb/a/r.md",
		Options: materialize.Options{Depth: 1, MaxNodes: materialize.DefaultMaxNodes, CrossKB: true},
	})
	require.NoError(t, err)
	require.Equal(t, materialize.RenderAgent(doc), body, "REST is byte-identical to the rendered plan")
	require.Equal(t, []string{"kb/a/r.md", "kb/a/b.md", "kb://" + beta12 + "/kb/b/x.md"}, []string{doc.Nodes[0].Path, doc.Nodes[1].Path, doc.Nodes[2].Path})
	require.Contains(t, body, "B first.", "pinned: R cites B's first version")
	require.Contains(t, body, "C (`c`, depth limit)")

	_, _, off := getMaterialize(t, h, base+"/facts/kb/a/r.md/materialize")
	require.Contains(t, off, "cross-KB references not requested", "cross_kb defaults to off")

	code, _, body = getMaterialize(t, h, base+"/commits/"+c1+"/facts/kb/a/b.md/materialize")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, "B first.")
	require.Contains(t, body, " at commit "+c1[:7]+".")
	require.Contains(t, body, "### C", "the commit-anchored document walks too")

	code, _, body = getMaterialize(t, h, base+"/facts/kb/a/b.md/materialize?unpin=true")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, "B second.")
	require.Contains(t, body, "references unpinned")
}

func TestMaterializeRoute_Refusals(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	writeMatFact(t, alpha, "agent/test", "kb/a/r.md", "R", "R body.")
	h := newTestRouter(m)
	base := "/repos/alpha/branches/agent:test/facts/"

	for _, q := range []string{"depth=11", "depth=x", "max_nodes=0", "max_nodes=501", "unpin=maybe", "deep=2"} {
		code, _, body := getMaterialize(t, h, base+"kb/a/r.md/materialize?"+q)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", q, body)
	}
	code, _, body := getMaterialize(t, h, base+"kb/a/nope.md/materialize")
	require.Equal(t, http.StatusNotFound, code, body)
	code, _, _ = getMaterialize(t, h, base+"kb/.drafts/x.md/materialize")
	require.Equal(t, http.StatusBadRequest, code, "a dot path stays closed to the fact routes")
}

// The lens route walks through the lens's own binding: a fact in a read
// mount is addressed kb://<id12>/… and its nodes stay qualified.
func TestMaterializeRoute_Lens(t *testing.T) {
	m := newMatLensManager(t, "w", "r")
	w, r := m.Get("w"), m.Get("r")
	createLens(t, m, "both", "w", "r")
	r12 := fact.ID12(r.ID())
	writeMatFact(t, r, r.AgentBranch(), "kb/m/y.md", "Y", "Y body.")
	writeMatFact(t, r, r.AgentBranch(), "kb/m/x.md", "X", "X body.", "kb/m/y.md")
	_ = w

	h := newTestRouter(m)
	code, _, body := getMaterialize(t, h, "/lenses/both/facts/kb:%2F%2F"+r12+"%2Fkb%2Fm%2Fx.md/materialize")
	require.Equal(t, http.StatusOK, code, body)
	require.True(t, strings.HasPrefix(body, "# X\n"), body)
	require.Contains(t, body, "Materialized from `kb://"+r12+"/kb/m/x.md`")
	require.Contains(t, body, "### Y")

	// Not a mount-topology oracle: an unmounted id and a missing fact in a
	// mounted repo answer byte-identically (both name only the requested
	// path, which differs, so compare with the path swapped out).
	_, _, unmounted := getMaterialize(t, h, "/lenses/both/facts/kb:%2F%2F0123456789ab%2Fkb%2Fm%2Fq.md/materialize")
	_, _, missing := getMaterialize(t, h, "/lenses/both/facts/kb:%2F%2F"+r12+"%2Fkb%2Fm%2Fq.md/materialize")
	require.Equal(t, strings.ReplaceAll(unmounted, "0123456789ab", r12), missing)
}
