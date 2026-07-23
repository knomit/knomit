package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReverseDependentPathsExported verifies that ReverseDependentPaths (the
// exported wrapper) returns the same result as the private reverseDependentPaths
// and includes the expected transitive dependent.
func TestReverseDependentPathsExported(t *testing.T) {
	ctx := context.Background()
	svc, branch := newBlastRadiusFixture(t)

	// A is the target; B derives from A via refs.
	writeFact(t, svc, branch, "kb/t/a.md", nil)
	writeFact(t, svc, branch, "kb/t/b.md", []string{"kb/t/a.md"})

	// Exported wrapper must include B as a dependent of A.
	deps, err := svc.gq.ReverseDependentPaths(ctx, "kb/t/a.md")
	require.NoError(t, err)
	_, hasB := deps["kb/t/b.md"]
	require.True(t, hasB, "ReverseDependentPaths must include dependent b: %v", deps)

	// Wrapper must be behavior-preserving: output equals the private method.
	private, err := svc.gq.reverseDependentPaths(ctx, "kb/t/a.md")
	require.NoError(t, err)
	require.Equal(t, private, deps, "exported wrapper must equal private reverseDependentPaths")
}
