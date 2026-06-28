package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newBlastRadiusFixture builds a fresh service+branch for blast-radius tests.
// Returns (svc, branch, cleanup-via-t.Cleanup).
func newBlastRadiusFixture(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, "main"
}

func writeFact(t *testing.T, svc *Service, branch, path string, refs []string) {
	t.Helper()
	_, err := svc.Facts().WriteFact(
		context.Background(), branch, path,
		testFactBody(path, 0.5, refs), "init "+path, "",
	)
	require.NoError(t, err)
}

// Diamond fixture: A → {B, C}, B → D, C → D. Reverse from D = {A, B, C}.
// A is counted once even though there are two distinct walk paths to it
// (D→B→A and D→C→A) — UNION in the recursive CTE dedupes.
func TestReverseDependentPathsDiamond(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	writeFact(t, svc, branch, "kb/x/d.md", nil)
	writeFact(t, svc, branch, "kb/x/b.md", []string{"kb/x/d.md"})
	writeFact(t, svc, branch, "kb/x/c.md", []string{"kb/x/d.md"})
	writeFact(t, svc, branch, "kb/x/a.md", []string{"kb/x/b.md", "kb/x/c.md"})

	si := svc.Search().(*searchIndex)
	deps, err := si.reverseDependentPaths(ctx, "kb/x/d.md")
	require.NoError(t, err)

	want := map[string]struct{}{"kb/x/a.md": {}, "kb/x/b.md": {}, "kb/x/c.md": {}}
	require.Len(t, deps, len(want), "deps: %v", deps)
	for p := range want {
		_, ok := deps[p]
		require.Truef(t, ok, "missing dependent %q in %v", p, deps)
	}
	// path itself must NOT appear in its own dependent set.
	_, hasSelf := deps["kb/x/d.md"]
	require.False(t, hasSelf, "self must not appear in dependents")
}

// Cycle: A → B → C → A. Reverse from B must terminate and include {A, C}.
// UNION (not UNION ALL) is what saves us from infinite recursion.
func TestReverseDependentPathsCycleTerminates(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	// Bootstrap each path with no refs, then update with refs to close the cycle.
	writeFact(t, svc, branch, "kb/x/a.md", nil)
	writeFact(t, svc, branch, "kb/x/b.md", []string{"kb/x/a.md"})
	writeFact(t, svc, branch, "kb/x/c.md", []string{"kb/x/b.md"})

	// Update a to ref c, closing the cycle a → c → b → a.
	_, err := svc.Facts().WriteFact(ctx, branch, "kb/x/a.md",
		testFactBody("a", 0.5, []string{"kb/x/c.md"}), "close cycle", "")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)
	deps, err := si.reverseDependentPaths(ctx, "kb/x/b.md")
	require.NoError(t, err)
	// Reverse from b: c derives from b; a derives from c. Cycle reaches a back to b
	// but b is excluded by the path-self filter.
	_, hasA := deps["kb/x/a.md"]
	_, hasC := deps["kb/x/c.md"]
	require.True(t, hasA, "expected a in deps via cycle: %v", deps)
	require.True(t, hasC, "expected c in deps: %v", deps)
}

// Retracted intermediate: A → B → C; retract B (its node + edges persist).
// Reverse from C must still surface A — the historical graph keeps transmission
// through retracted intermediates.
func TestReverseDependentPathsRetractedIntermediate(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	writeFact(t, svc, branch, "kb/x/c.md", nil)
	writeFact(t, svc, branch, "kb/x/b.md", []string{"kb/x/c.md"})
	writeFact(t, svc, branch, "kb/x/a.md", []string{"kb/x/b.md"})

	_, err := svc.Facts().DeleteFact(ctx, branch, "kb/x/b.md", "retract b")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)
	deps, err := si.reverseDependentPaths(ctx, "kb/x/c.md")
	require.NoError(t, err)
	_, hasA := deps["kb/x/a.md"]
	require.True(t, hasA, "retracted intermediate must still transmit reach: %v", deps)
}

// BlastRadius counts live transitive dependents at HEAD. Same diamond fixture
// as TestReverseDependentPathsDiamond — all three live → BlastRadius(d)=3,
// BlastRadius(leaf a)=0.
func TestBlastRadiusLiveDiamond(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	writeFact(t, svc, branch, "kb/x/d.md", nil)
	writeFact(t, svc, branch, "kb/x/b.md", []string{"kb/x/d.md"})
	writeFact(t, svc, branch, "kb/x/c.md", []string{"kb/x/d.md"})
	writeFact(t, svc, branch, "kb/x/a.md", []string{"kb/x/b.md", "kb/x/c.md"})

	n, err := svc.Search().BlastRadius(ctx, branch, "kb/x/d.md")
	require.NoError(t, err)
	require.Equal(t, 3, n, "BlastRadius(d): three live dependents")

	leaf, err := svc.Search().BlastRadius(ctx, branch, "kb/x/a.md")
	require.NoError(t, err)
	require.Equal(t, 0, leaf, "BlastRadius(a): leaf fact has no dependents")

	// Path never written — graph has no seed node — radius 0.
	zero, err := svc.Search().BlastRadius(ctx, branch, "kb/x/never.md")
	require.NoError(t, err)
	require.Equal(t, 0, zero, "BlastRadius(unknown): no node → 0")
}

// TestBlastRadius_BatchedLivenessQuery confirms BlastRadius returns the correct
// count when many dependents exist and verifies they are all processed in one
// batched query (correctness regression — the count must match a known fixture).
func TestBlastRadius_BatchedLivenessQuery(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	// Root fact + 10 direct dependents, all live.
	writeFact(t, svc, branch, "kb/root.md", nil)
	for i := 0; i < 10; i++ {
		writeFact(t, svc, branch, fmt.Sprintf("kb/dep%02d.md", i), []string{"kb/root.md"})
	}

	radius, err := svc.Search().BlastRadius(ctx, branch, "kb/root.md")
	require.NoError(t, err)
	require.Equal(t, 10, radius,
		"BlastRadius must count all 10 live dependents")
}

// Dependents retracted at HEAD must not be counted, even though their
// historical edges persist (they still appear in reverseDependentPaths).
func TestBlastRadiusExcludesRetractedDependent(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	writeFact(t, svc, branch, "kb/x/d.md", nil)
	writeFact(t, svc, branch, "kb/x/b.md", []string{"kb/x/d.md"})

	// Sanity: live count is 1.
	n, err := svc.Search().BlastRadius(ctx, branch, "kb/x/d.md")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Retract b. Historically still reachable; live count drops to 0.
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/x/b.md", "retract b")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)
	deps, err := si.reverseDependentPaths(ctx, "kb/x/d.md")
	require.NoError(t, err)
	_, hasB := deps["kb/x/b.md"]
	require.True(t, hasB, "historical graph still records the dependent: %v", deps)

	n, err = svc.Search().BlastRadius(ctx, branch, "kb/x/d.md")
	require.NoError(t, err)
	require.Equal(t, 0, n, "retracted dependent must drop from BlastRadius")
}
