package fact

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// withContext returns a copy of v carrying ctx (a fresh map, so cases never
// share one).
func withContext(v mv, ctx map[string]any) mv {
	v.context = CopyContext(ctx)
	return v
}

// F22 / F20: context merges KEY BY KEY, three-way. Side A changes task, side
// B changes verdict: both changes are kept and the strategy decides nothing.
//
// SABOTAGE: taking the winner's whole map (the dedup rule) loses one of the
// two changes → red.
func TestMergeVersions_Context_EachSideChangesADifferentKey(t *testing.T) {
	base := withContext(baseVersion(), map[string]any{"task": "t-1", "verdict": "agree"})
	src := withContext(base, map[string]any{"task": "t-2", "verdict": "agree"})
	dst := withContext(base, map[string]any{"task": "t-1", "verdict": "disagree"})
	dst.conf = 0.9 // dst is the strategy's winner: it must not matter
	f, rec, _ := mustMerge(t, base.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{})
	require.Equal(t, map[string]any{"task": "t-2", "verdict": "disagree"}, f.Context)
	require.NotContains(t, strings.Join(rec.Decided, ","), "context", "no key was changed by both sides")
}

// Both sides change verdict differently: the strategy's winner takes it and
// Decided names the key — the NAME, never the value (the trailer is a field
// list). Under merge the more confident side wins; under merge_consensus the
// consensus side wins, whatever its confidence.
//
// SABOTAGE: swap the sides in mergeContext → the loser's value lands → red.
func TestMergeVersions_Context_BothChangeAKey(t *testing.T) {
	base := withContext(baseVersion(), map[string]any{"verdict": "unsure"})
	src := withContext(base, map[string]any{"verdict": "agree"})
	dst := withContext(base, map[string]any{"verdict": "disagree"})
	src.conf = 0.9

	f, rec, _ := mustMerge(t, base.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{Rule: MergeConfidence})
	require.Equal(t, "agree", f.Context["verdict"], "merge: the more confident side (src)")
	require.Equal(t, MergeSrc, rec.Winner)
	require.Contains(t, rec.Decided, "context.verdict")
	for _, d := range rec.Decided {
		require.NotContains(t, d, "agree", "Decided carries key names, never values")
		require.NotContains(t, d, "disagree")
	}

	f, rec, _ = mustMerge(t, base.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{Rule: MergeTakeConsensus, Consensus: MergeDst})
	require.Equal(t, "disagree", f.Context["verdict"], "merge:consensus: the consensus side (dst), though less confident")
	require.True(t, slices.Contains(rec.Decided, "context.verdict"))
}

// One side deletes a key the other left alone: deleted. One side adds a key:
// added. Both make the same change: taken, not decided.
func TestMergeVersions_Context_DeleteAddSame(t *testing.T) {
	base := withContext(baseVersion(), map[string]any{"task": "t-1", "note": "x", "score": 0.5})
	src := withContext(base, map[string]any{"task": "t-1", "score": 0.7})                             // deleted note, changed score
	dst := withContext(base, map[string]any{"task": "t-1", "note": "x", "score": 0.7, "final": true}) // added final, same score change
	f, rec, _ := mustMerge(t, base.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{})
	require.Equal(t, map[string]any{"task": "t-1", "score": 0.7, "final": true}, f.Context)
	require.NotContains(t, strings.Join(rec.Decided, ","), "context")

	// Both sides delete everything: no context, no empty map written.
	src = withContext(base, nil)
	dst = withContext(base, nil)
	f, _, out := mustMerge(t, base.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{})
	require.Nil(t, f.Context)
	require.NotContains(t, string(out), "context")
}

// A version holding context is no longer "lossy": before F22 the unknown key
// made the merge fall back to a side-pick. A version whose map ParseFact had
// to DROP still is.
func TestMergeVersions_Context_LossyOnlyWhenMalformed(t *testing.T) {
	base := withContext(baseVersion(), map[string]any{"task": "t-1"})
	src := withContext(base, map[string]any{"task": "t-2"})
	_, _, ok := MergeVersions(mvPath, base.bytes(t), src.bytes(t), base.bytes(t), MergeStrategy{})
	require.True(t, ok, "a well-formed context merges")

	bad := strings.Replace(string(src.bytes(t)), "context: {task: t-2}", "context: {task: [t-2]}", 1)
	require.Contains(t, bad, "[t-2]", "fixture: the map was made malformed")
	_, rec, ok := MergeVersions(mvPath, base.bytes(t), []byte(bad), base.bytes(t), MergeStrategy{})
	require.False(t, ok)
	require.Equal(t, "src-lossy", rec.Reason, "a dropped map would be lost by a merge")
}
