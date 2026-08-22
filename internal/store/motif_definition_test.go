package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Definitions: one per cluster, keyed on the STABLE cluster key, with staleness
// derived from membership rather than flagged by whoever changed it.

func TestMotifDefinitions_UndefinedClustersNeedDefining(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Len(t, need, 2, "every cluster starts undefined")

	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, need[0].ClusterKey,
		"A component continues serving after a dependency fails, without signalling."))

	need, err = svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Len(t, need, 1, "a defined cluster drops out of the queue")
}

// Staleness is a COMPARISON against current membership, not a flag. This is the
// property that makes it catch every cause of drift rather than the one cause
// whoever wrote the flag remembered.
func TestMotifDefinitions_MembershipChangeMakesADefinitionStale(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence."))

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, need, "precondition: the definition is current")

	// A new spelling joins the cluster MECHANICALLY — no judge merge involved.
	// A flag set by the merge path would have missed this entirely.
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	need, err = svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Len(t, need, 1,
		"a cluster that gained a member covers more than its definition was authored over")
	require.Equal(t, key, need[0].ClusterKey)
}

// The representative flipping is NOT a membership change. This is the whole
// point of keying on cluster_key: a definition must not be re-authored because
// usage shifted between two spellings of the same mechanism.
func TestMotifDefinitions_RepresentativeFlipDoesNotStaleADefinition(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"atomic-write"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"write-atomic"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	key, err := svc.Motifs().ClusterKey(ctx, branch, "atomic-write")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence."))

	repBefore, err := svc.Motifs().CanonicalID(ctx, branch, "atomic-write")
	require.NoError(t, err)

	// Shift usage so the OTHER spelling leads. Membership is unchanged.
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"write-atomic"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	repAfter, err := svc.Motifs().CanonicalID(ctx, branch, "atomic-write")
	require.NoError(t, err)
	require.NotEqual(t, repBefore, repAfter, "precondition: the representative must actually flip")

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, need,
		"a representative flip changes the LABEL, not the cluster — re-authoring here "+
			"would be paying an LLM to restate the same sentence")
}

// The interim rule: a merged cluster KEEPS the survivor's definition and is
// queued for refresh. Gapping it would be worse than a slightly wide sentence,
// since a judge merge asserts the phrasings name the same mechanism.
func TestMotifDefinitions_MergedClusterKeepsAnInterimDefinition(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	keyA, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	keyB, err := svc.Motifs().ClusterKey(ctx, branch, "quiet-degradation")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyA, "Definition of A."))
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyB, "Definition of B."))

	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation", "both name serving on after a dependency fails"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	survivor, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)

	def, ok, err := svc.Motifs().Definition(ctx, branch, survivor)
	require.NoError(t, err)
	require.True(t, ok,
		"the merged cluster must keep an INTERIM definition rather than gapping — the "+
			"merge asserted the two name one mechanism, so the survivor's sentence is "+
			"approximately right for the union")
	require.NotEmpty(t, def)

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	require.Len(t, need, 1, "...and it must be QUEUED for refresh, not left as final")
	require.Equal(t, survivor, need[0].ClusterKey)
}

// MN3 holds here too: authoring definitions must not touch a fact.
func TestMotifDefinitions_MN3_TouchesNoFact(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	rec, err := svc.FactQuery().GetByPath(ctx, branch, "kb/alpha/one.md")
	require.NoError(t, err)
	before := rec.BlobHash

	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence."))

	rec, err = svc.FactQuery().GetByPath(ctx, branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.Equal(t, before, rec.BlobHash, "MN3: defining a motif must never rewrite a fact")
}

// A definition for a cluster that has left the corpus must not resurface. The
// row may linger as history; the queue and the lookup are about live clusters.
func TestMotifDefinitions_VanishedClusterIsNotOffered(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	key, err := svc.Motifs().ClusterKey(ctx, branch, "config-drift")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence."))

	// The mechanism leaves the corpus.
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	for _, c := range need {
		require.NotEqual(t, key, c.ClusterKey,
			"a cluster no live fact carries must not be queued for definition")
	}
}
