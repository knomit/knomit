package materialize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
)

// B1 (engine half). FactSetFirstPage refuses a binding that reads more than
// its write repo with ErrInvalid, before any search, planning slot or session
// row. SUPERSEDED by the lens facade tests (F3), which lift this refusal.
//
// The control binding has the same write repo and the same data but ONE
// mount; it is served, so the refusal is the multi-member guard and not the
// fixture.
//
// Sabotage: delete the req.Binding.IsLens() refusal in FactSetFirstPage; the
// multi-member call is served (a page, a session row) and the ErrInvalid and
// write-count assertions fail.
func TestFactSet_Contract_MultiMemberBindingIsRefusedBeforeAnyWork(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/u/a.md", "A", "a body.")

	svc2, err := store.Open(filepath.Join(t.TempDir(), "beta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc2.Close() })
	require.NoError(t, svc2.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	beta := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "beta", UID: "uid-beta", AgentBranch: "agent/test", Svc: svc2, OntologyRoot: "kb",
	})

	lens := repos.NewBindingForTest(f.ri,
		repos.ReadTarget{RI: f.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: beta, Branch: "agent/test"})
	require.True(t, lens.IsLens(), "two distinct mounts: a lens")

	ti := f.svc.ToolSession()
	before := ti.FactSetWrites()
	req := f.request("kb/u/")
	req.Binding = lens
	_, _, err = FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualError(t, err, "invalid materialize request: a factset materializes one repository's search; this binding is a lens")
	require.Equal(t, before, ti.FactSetWrites(), "refused before the session row is written")

	// Control: one mount over the same write repo is served.
	one := repos.NewBindingForTest(f.ri, repos.ReadTarget{RI: f.ri, Branch: "agent/test"})
	require.False(t, one.IsLens())
	req.Binding = one
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 1, p.Units)
	require.Equal(t, "kb/u/a.md", p.Facts[0].Path)
	require.Equal(t, before+1, ti.FactSetWrites(), "the served call wrote its one row")
}
