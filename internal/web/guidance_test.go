package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// F23 (REST half): on a repo with no origin, a person's ontology and guidance
// file reach main through git; the REST fact doors then try to write the
// declared-but-absent .knomit/guidance/x.md and to rewrite or delete the
// person's file. After a reconcile round, the guidance knomit reads at the
// consensus tip is exactly the person's.
func TestGuidance_RESTCannotWriteGuidance(t *testing.T) {
	m := contextManager(t, "alpha")
	ri := m.Get("alpha")
	r := (&Server{Manager: m, AgentBranch: "machine/test", OntologyRoot: "kb"}).NewAPIRouter()
	b := urlBranch(ri.AgentBranch())
	h := threeRootsREST{m: m, ri: ri, r: r, b: b}
	ctx := context.Background()

	advance := func() {
		t.Helper()
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			rem, err := svc.Remote().GetRemote("origin")
			require.NoError(t, err)
			require.Nil(t, rem, "the case under test is a repo with no origin")
			_, err = svc.AdvanceLocalUpstream(ctx, ri.AgentBranch(), svc.UpstreamBranch())
			require.NoError(t, err)
		}))
	}
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		for p, c := range map[string]string{
			".knomit/ontology.yaml":        "id: x\nname: X\ntopics:\n  forecast:\n    description: F.\n    guidance:\n      hypothesize: guidance/forecast.md\n      review: guidance/x.md\n",
			".knomit/guidance/forecast.md": "PERSON TEXT\n",
		} {
			_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), p, c, "git "+p, "updated")
			require.NoError(t, err)
		}
	}))
	advance()
	g := ri.ConsensusGuidance(ctx)
	require.NotNil(t, g)

	before := h.tip(t)
	for _, p := range []string{".knomit/guidance/x.md", ".knomit/guidance/forecast.md"} {
		rec := h.do(t, http.MethodPut, "/repos/alpha/branches/"+b+"/facts/"+p, putBody(t, ""))
		require.Equalf(t, http.StatusBadRequest, rec.Code, "PUT %s: %s", p, rec.Body.String())
		rec = h.do(t, http.MethodDelete, "/repos/alpha/branches/"+b+"/facts/"+p, "")
		require.Equalf(t, http.StatusBadRequest, rec.Code, "DELETE %s: %s", p, rec.Body.String())
	}
	require.Equal(t, before, h.tip(t), "no REST door moved the agent tip")

	seedVerdict(t, ri, "kb/verdicts/after.md", "")
	advance()
	g2 := ri.ConsensusGuidance(ctx)
	require.NotNil(t, g2)
	require.NotEqual(t, g.Commit, g2.Commit, "the reconcile round moved the consensus tip")
	txt, ok := g2.Text("guidance/forecast.md")
	require.True(t, ok)
	require.Equal(t, "PERSON TEXT\n", txt)
	_, ok = g2.Text("guidance/x.md")
	require.False(t, ok)
}
