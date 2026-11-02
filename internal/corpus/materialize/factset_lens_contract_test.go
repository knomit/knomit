package materialize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
)

// B1 (engine half), as the lens facade (F3) rules it. This test replaced PR
// A's refusal test ("a factset materializes one repository's search; this
// binding is a lens"): a binding that reads more than its write repo is now
// served by the facade, with ONE session row in the write repo's session DB
// and none in the read member's; its roots carry v2's per-member layout. The
// control binding has the same write repo and ONE mount: it stays on the repo
// path (a v1 row, no anchors map), so the dispatch is the IsLens guard and not
// the fixture.
//
// Sabotage: route the IsLens binding to the repo path (drop the dispatch in
// FactSetFirstPage): the lens call writes a v1 row and serves only the write
// repo's root, and the v2 / roots assertions fail.
func TestFactSet_Contract_MultiMemberBindingIsTheLensFacade(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	f.write(t, "kb/u/a.md", "A", "a body.")

	svc2, err := store.Open(filepath.Join(t.TempDir(), "beta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc2.Close() })
	require.NoError(t, svc2.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	beta := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "beta", UID: "uid-beta", AgentBranch: "agent/test", Svc: svc2, OntologyRoot: "kb",
	})
	bf := &factsetFixture{ri: beta, svc: svc2, b: repos.NewBindingOfRepo(beta, "agent/test")}
	bf.write(t, "kb/u/b.md", "B", "b body.")

	lens := repos.NewBindingForTest(f.ri,
		repos.ReadTarget{RI: f.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: beta, Branch: "agent/test"})
	require.True(t, lens.IsLens(), "two distinct mounts: a lens")

	ti := f.svc.ToolSession()
	before, beforeBeta := ti.FactSetWrites(), svc2.ToolSession().FactSetWrites()
	req := f.request("kb/u/")
	req.Binding = lens
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 2, p.Units, "both members' results")
	require.ElementsMatch(t, []string{"kb/u/a.md", "kb://" + bf.id12() + "/kb/u/b.md"}, paths(unitRoots(allPages(t, f.scope("o"), lens, p)...)))
	require.Len(t, p.Anchors, 2)
	require.Equal(t, before+1, ti.FactSetWrites(), "ONE row, in the write repo's session DB")
	require.Equal(t, beforeBeta, svc2.ToolSession().FactSetWrites(), "no row in the read member's")
	require.Equal(t, frozenVersionLens, lensFrozenOf(t, f.scope("o"), *p.Cursor).V)

	// Control: one mount over the same write repo is the repo path.
	one := repos.NewBindingForTest(f.ri, repos.ReadTarget{RI: f.ri, Branch: "agent/test"})
	require.False(t, one.IsLens())
	req.Binding = one
	p, _, err = FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p.Units)
	require.Equal(t, "kb/u/a.md", p.Facts[0].Path)
	require.Nil(t, p.Anchors, "a repo page has no anchors map")
	_, fr := frozenOf(t, f.scope("o"), *p.Cursor)
	require.Equal(t, frozenVersion, fr.V)
	require.Equal(t, before+2, ti.FactSetWrites())
}
