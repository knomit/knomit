package store

import (
	"testing"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/stretchr/testify/require"
)

// TestBodyDelta_UnifiedNeverShorterThanBound pins minUnifiedDiffLen: the
// shortest unified diffs difflib can emit (one empty line removed, added or
// changed) are all longer than the bound, so skipping the unified diff for a
// magnitude within the bound never changes bodyDelta's answer.
func TestBodyDelta_UnifiedNeverShorterThanBound(t *testing.T) {
	for _, c := range [][2]string{{"\n", ""}, {"", "\n"}, {"\n", "x\n"}, {"a", "b"}, {"a\n", "b\n"}, {"a", ""}, {"", "a"}, {"x\ny\n", "x\n"}} {
		u, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(c[0]), B: difflib.SplitLines(c[1]), Context: 1})
		require.NoError(t, err)
		require.Greater(t, len(u), minUnifiedDiffLen, "%q -> %q gave %q", c[0], c[1], u)
	}
	require.Equal(t, "+1/-1", bodyDelta("a\n", "b\n"))
	require.Equal(t, "", bodyDelta("same", "same"))
}
