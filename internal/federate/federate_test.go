package federate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFuseRRF(t *testing.T) {
	// N=1: identity order (the RFC's N=1 no-behavior-change invariant).
	got := FuseRRF([]int{3})
	require.Equal(t, []MountRef{{0, 0}, {0, 1}, {0, 2}}, got)

	// Two lists [3,2]: rank layers, mount order within a layer.
	got = FuseRRF([]int{3, 2})
	require.Equal(t, []MountRef{
		{0, 0}, {1, 0}, // rank 0 layer
		{0, 1}, {1, 1}, // rank 1 layer
		{0, 2}, // rank 2 layer
	}, got)

	// Empty lists mixed with non-empty.
	got = FuseRRF([]int{0, 2, 0})
	require.Equal(t, []MountRef{{1, 0}, {1, 1}}, got)

	// All-empty → empty.
	require.Empty(t, FuseRRF([]int{0, 0}))
	require.Empty(t, FuseRRF(nil))
}

func TestMergeRecent(t *testing.T) {
	// [[100,50],[70]] → committed_at DESC: 100(0,0), 70(1,0), 50(0,1).
	got := MergeRecent([][]int64{{100, 50}, {70}}, 10)
	require.Equal(t, []MountRef{{0, 0}, {1, 0}, {0, 1}}, got)

	// cap max=2 truncates.
	got = MergeRecent([][]int64{{100, 50}, {70}}, 2)
	require.Equal(t, []MountRef{{0, 0}, {1, 0}}, got)

	// Equal stamps across mounts → mount order, then per-mount order.
	got = MergeRecent([][]int64{{100, 100}, {100}}, 10)
	require.Equal(t, []MountRef{{0, 0}, {0, 1}, {1, 0}}, got)
}

func TestID12(t *testing.T) {
	require.Equal(t, "3f9a2c1e8b7d", ID12("3f9a2c1e8b7d0000000000000000000000000000"))
	// Short input returned as-is.
	require.Equal(t, "abc", ID12("abc"))
	require.Equal(t, "3f9a2c1e8b7d", ID12("3f9a2c1e8b7d"))
}

func TestQualifyPath(t *testing.T) {
	require.Equal(t, "kb://3f9a2c1e8b7d/kb/a/b.md", QualifyPath("3f9a2c1e8b7d", "kb/a/b.md"))
}

func TestParseQualifiedPath(t *testing.T) {
	// Bare path → not qualified.
	id, rel, qualified, err := ParseQualifiedPath("kb/a/b.md")
	require.NoError(t, err)
	require.False(t, qualified)
	require.Equal(t, "", id)
	require.Equal(t, "kb/a/b.md", rel)

	// Well-formed qualified path.
	id, rel, qualified, err = ParseQualifiedPath("kb://3f9a2c1e8b7d/kb/a/b.md")
	require.NoError(t, err)
	require.True(t, qualified)
	require.Equal(t, "3f9a2c1e8b7d", id)
	require.Equal(t, "kb/a/b.md", rel)

	// Malformed variants.
	for _, p := range []string{
		"kb://short/x.md",
		"kb://GGGGGGGGGGGG/x.md",
		"kb://3f9a2c1e8b7d",  // no rel
		"kb://3f9a2c1e8b7d/", // empty rel
	} {
		_, _, qualified, err := ParseQualifiedPath(p)
		require.True(t, qualified, "%q is a kb:// path", p)
		require.Error(t, err, "%q must be rejected", p)
	}
}

func TestTopicOfPathFilter(t *testing.T) {
	require.Equal(t, "decisions", topicOfPathFilter("kb/decisions/lens/"))
	require.Equal(t, "decisions", topicOfPathFilter("kb/decisions/x.md"))
	// Prefix filter, topic segment NOT delimited → no constraint.
	require.Equal(t, "", topicOfPathFilter("kb/decisions"))
	require.Equal(t, "", topicOfPathFilter("kb/"))
	require.Equal(t, "", topicOfPathFilter("kb"))
	require.Equal(t, "", topicOfPathFilter(""))
	require.Equal(t, "", topicOfPathFilter("other/x"))
}
