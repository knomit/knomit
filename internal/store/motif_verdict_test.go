package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Judge verdicts: what the pass has already decided about a pair of clusters.
// Two consumers — the rebuild overlay unions the merges, the pair selector
// skips anything already answered.

// "Name the shared mechanism or it does not count" has to be enforced at the
// write path. A rule the caller can decline to follow is a convention, not a
// guard, and the rationale is the audit trail a later harden pass reads.
func TestMotifVerdict_MergeWithoutRationaleIsRefused(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	err := svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "")
	require.Error(t, err, "a merge with no named mechanism must be refused, not silently accepted")

	err = svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "   ")
	require.Error(t, err, "whitespace is not a named mechanism")
}

// The rationale reaches the alias rows of the cluster it formed, so a reader of
// method='judge' can see WHY without joining back to the verdict table.
func TestMotifVerdict_RationaleReachesTheAliasRow(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation",
		"both name a component continuing to serve after a dependency failed, without signalling"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	rows, err := svc.Motifs().AliasRows(ctx, branch)
	require.NoError(t, err)
	row, ok := rows["silent-fallback"]
	require.True(t, ok)
	require.Equal(t, "judge", row.Method)
	require.Contains(t, row.Rationale, "without signalling")
}

// A decline is recorded so the pair is not re-offered every session. Without
// this the pass is only half incremental: merges are cheap, rejections are
// re-litigated forever.
func TestMotifVerdict_DeclinedPairIsNotReOffered(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	keyA, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	keyB, err := svc.Motifs().ClusterKey(ctx, branch, "config-drift")
	require.NoError(t, err)

	answered, err := svc.Motifs().AnsweredPairs(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, answered, "nothing decided yet")

	require.NoError(t, svc.Motifs().RecordJudgeDecline(ctx, branch, "silent-fallback", "config-drift"))

	answered, err = svc.Motifs().AnsweredPairs(ctx, branch)
	require.NoError(t, err)
	require.Contains(t, answered, pairKey(keyA, keyB))

	// A decline must NOT merge anything.
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "config-drift")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

// A verdict binds only while both clusters still mean what they meant. New
// carrier spellings can genuinely change the answer, so a membership change
// re-eligibilizes the pair — the same structural expiry as Phase 0 keying its
// verdicts on content-addressed fact ids.
func TestMotifVerdict_MembershipChangeReEligibilizes(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeDecline(ctx, branch, "silent-fallback", "config-drift"))

	keyA, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	keyB, err := svc.Motifs().ClusterKey(ctx, branch, "config-drift")
	require.NoError(t, err)

	answered, err := svc.Motifs().AnsweredPairs(ctx, branch)
	require.NoError(t, err)
	require.Contains(t, answered, pairKey(keyA, keyB), "precondition: the decline is binding")

	// A new spelling joins the first cluster — it now covers more than the
	// judge was shown.
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	answered, err = svc.Motifs().AnsweredPairs(ctx, branch)
	require.NoError(t, err)
	require.NotContains(t, answered, pairKey(keyA, keyB),
		"a cluster that gained a member is not the cluster the judge declined")
}
