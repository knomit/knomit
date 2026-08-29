package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// ClustersUnder is the vocabulary OF A SUBTREE: a cluster no fact under the
// prefix carries is absent, not present with a zero, so a reader standing in a
// folder is never offered a shape that would filter the folder to nothing.
func TestClustersUnder_DropsClustersWithNoCarrierInScope(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/decisions/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/decisions/two.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/gotchas/three.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	all, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, all, 2, "the branch-wide vocabulary holds both")

	scoped, err := svc.Motifs().ClustersUnder(ctx, branch, "kb/decisions")
	require.NoError(t, err)
	require.Len(t, scoped, 1, "config-drift has no carrier under kb/decisions")
	require.Equal(t, "silent-fallback", scoped[0].CanonicalID)
}

// DF is scoped and DFTotal is not — the pair exists because the pivot the row
// opens DROPS the path, so the row has to be able to say both how much of the
// shape is here and how much the pivot will return.
func TestClustersUnder_DFIsScopedAndDFTotalIsNot(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/decisions/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/decisions/two.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/gotchas/three.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	scoped, err := svc.Motifs().ClustersUnder(ctx, branch, "kb/decisions")
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, 2, scoped[0].DF, "carriers under the prefix")
	require.Equal(t, 3, scoped[0].DFTotal, "carriers on the whole branch")
}

// An empty prefix is exactly Clusters, DFTotal included: the unscoped call is
// not a special case with its own answer, it is this one with no narrowing.
func TestClustersUnder_EmptyPrefixEqualsClusters(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/decisions/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/gotchas/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	all, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	unscoped, err := svc.Motifs().ClustersUnder(ctx, branch, "")
	require.NoError(t, err)
	require.Equal(t, all, unscoped)
	for _, c := range all {
		require.Equal(t, c.DF, c.DFTotal, "unscoped, the two counts are one number")
	}
}

// Membership and the elected canonical are properties of the CLUSTER, not of
// where the reader is standing. If the election ran over in-scope spellings
// only, one folder would call a cluster `silent-fallback` and another would
// call the same cluster `silent-fallbacks` — and the pivot heading, which is
// branch-wide, would disagree with the row that opened it.
func TestClustersUnder_CanonicalAndMembersStayBranchWide(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	// `silent-fallbacks` leads branch-wide (2 carriers to 1) but is absent from
	// kb/decisions, where only `silent-fallback` is used.
	writeMotifFact(t, svc, branch, "kb/gotchas/one.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/gotchas/two.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/decisions/three.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	all, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, all, 1)

	scoped, err := svc.Motifs().ClustersUnder(ctx, branch, "kb/decisions")
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, all[0].CanonicalID, scoped[0].CanonicalID,
		"the scoped view must not rename a cluster the branch has already named")
	require.Equal(t, "silent-fallbacks", scoped[0].CanonicalID)
	require.Equal(t, all[0].Members, scoped[0].Members,
		"every spelling stays a member — the pivot passes them all, and it is branch-wide")
	require.Equal(t, all[0].ClusterKey, scoped[0].ClusterKey)
	require.Equal(t, 1, scoped[0].DF)
	require.Equal(t, 3, scoped[0].DFTotal)
}

// The health strip sits a few pixels from a count that IS scoped, so it has to
// be counted over the same facts the list is.
func TestVocabularyHealthUnder_ScopesToTheSubtree(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFactOrigin(t, svc, branch, "kb/decisions/one.md", []string{"silent-fallback"}, fact.Authored)
	writeMotifFactOrigin(t, svc, branch, "kb/decisions/two.md", []string{"silent-fallback"}, fact.Authored)
	writeMotifFactOrigin(t, svc, branch, "kb/gotchas/three.md", []string{"config-drift"}, fact.Authored)
	writeMotifFactOrigin(t, svc, branch, "kb/gotchas/four.md", []string{"config-drift"}, fact.Authored)
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	whole, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 2, whole.Clusters)
	require.Equal(t, 2, whole.Recurring)

	scoped, err := svc.Motifs().VocabularyHealthUnder(ctx, branch, "kb/decisions")
	require.NoError(t, err)
	require.Equal(t, 1, scoped.Clusters, "only the shapes carried here")
	require.Equal(t, 1, scoped.Recurring)
	require.Equal(t, 1, scoped.Links, "one use after the first, counted in scope")

	unscoped, err := svc.Motifs().VocabularyHealthUnder(ctx, branch, "")
	require.NoError(t, err)
	require.Equal(t, whole, unscoped, "an empty prefix is exactly VocabularyHealth")
}
