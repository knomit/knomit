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
		"A component continues serving after a dependency fails, without signalling.", DefinitionStamp{}))

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
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence.", DefinitionStamp{}))

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
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence.", DefinitionStamp{}))

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
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyA, "Definition of A.", DefinitionStamp{}))
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyB, "Definition of B.", DefinitionStamp{}))

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
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence.", DefinitionStamp{}))

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
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key, "A generic sentence.", DefinitionStamp{}))

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

// M2: a definition authored in the SAME session as a judge merge must be
// stamped with the membership it was AUTHORED AGAINST, not the membership the
// cluster has by the time it is written.
//
// The race has a real window: the define payload is built during Plan, and the
// alias item's verdicts are applied later in the same session. Reading
// membership at write time marked a pre-merge definition as current for a
// post-merge cluster — defeating the staleness comparison for precisely the
// merge case it was built to catch, and permanently, since nothing would ever
// re-queue it.
func TestMotifDefinitions_SameSessionMergeDoesNotMarkAStaleDefinitionCurrent(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	// Plan time: the pass is handed a target for the PRE-merge cluster.
	targets, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	// Target the cluster that will SURVIVE the merge (the key that wins min()).
	// A definition authored for the ABSORBED key is a different case: that
	// cluster ceases to exist, and its row is correctly retired rather than
	// re-queued — which is what the orphan test below covers. Choosing the
	// wrong side here made this test fail against a correct fix, which is
	// exactly how a fixture hides the behaviour it means to check.
	var target DefinitionTarget
	for _, c := range targets {
		if c.Name == "quiet-degradation" {
			target = c
		}
	}
	require.NotEmpty(t, target.ClusterKey, "fixture must offer the cluster to define")
	require.NotEmpty(t, target.Members, "the target must carry the membership it was selected with")

	// Later in the same session: a judge merge changes the cluster.
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation", "both name serving on after a failure"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	// The definition comes back, authored for what the pass was SHOWN.
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch,
		target.ClusterKey, "A sentence written for the pre-merge cluster.",
		DefinitionStamp{Members: target.Members, Known: true}))

	// It must be recognised as stale, because the cluster it describes has
	// changed underneath it.
	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	var queued bool
	for _, c := range need {
		if c.Interim == "A sentence written for the pre-merge cluster." {
			queued = true
		}
	}
	require.True(t, queued,
		"a definition authored before a same-session merge must be re-queued — "+
			"stamping it with post-merge membership marks it current forever")
}

// A judge merge leaves the ABSORBED key with no members. Its definition row
// must not linger: nothing queues or serves a cluster outside the vocabulary,
// so the row is invisible, accumulating, and liable to be resurrected if that
// key ever returns meaning something else.
func TestMotifDefinitions_OrphanedRowsAreRetiredOnRebuild(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	keyA, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	keyB, err := svc.Motifs().ClusterKey(ctx, branch, "quiet-degradation")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyA, "Definition of A.", DefinitionStamp{}))
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, keyB, "Definition of B.", DefinitionStamp{}))

	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation", "one mechanism"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	survivor, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	absorbed := keyA
	if survivor == keyA {
		absorbed = keyB
	}

	_, ok, err := svc.Motifs().Definition(ctx, branch, absorbed)
	require.NoError(t, err)
	require.False(t, ok, "the absorbed key's definition must be retired, not left orphaned")

	_, ok, err = svc.Motifs().Definition(ctx, branch, survivor)
	require.NoError(t, err)
	require.True(t, ok, "...while the survivor keeps its interim definition, as ruled")
}

// m2. An UNRESOLVED cluster genuinely has no recorded membership, and a caller
// that carries that answer must have it stamped as given.
//
// The empty string used to mean "I have nothing, read the current membership",
// so a pass holding a real-but-empty membership silently got read-at-write-time
// — the exact behaviour the stamp was introduced to remove, reappearing for the
// corpora least able to afford it (alias table unbuilt, or the Plan rebuild
// failed and was logged-and-continued).
//
// The two cases must produce DIFFERENT results here, or the test is not about
// the distinction: current membership is non-empty, so stamping "" leaves the
// definition stale and re-queued, while re-reading marks it current forever.
func TestMotifDefinitions_AnEmptyMembershipIsAnAnswerWhenTheCallerSaysSo(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)

	// The precondition that makes the two cases distinguishable.
	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	var target DefinitionTarget
	for _, c := range need {
		if c.ClusterKey == key {
			target = c
		}
	}
	require.NotEmpty(t, target.Members,
		"precondition: this cluster's CURRENT membership is non-empty, so an empty "+
			"stamp and a re-read cannot coincide")

	// The caller's answer is "no membership", and it means it.
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key,
		"A component keeps serving after a dependency fails.",
		DefinitionStamp{Members: "", Known: true}))

	need, err = svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	var queued bool
	for _, c := range need {
		if c.ClusterKey == key {
			queued = true
		}
	}
	require.True(t, queued,
		"a definition stamped with an EMPTY membership must read as stale against a "+
			"non-empty one. If the empty stamp were quietly replaced by current "+
			"membership, this cluster would be marked current and never refreshed")
}

// The other half: a caller with nothing to say still gets current membership,
// which is what keeps a definition written outside a pass from being born stale.
func TestMotifDefinitions_NoStampReadsCurrentMembership(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.NoError(t, svc.Motifs().PutDefinition(ctx, branch, key,
		"A component keeps serving after a dependency fails.", DefinitionStamp{}))

	need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch)
	require.NoError(t, err)
	for _, c := range need {
		require.NotEqualf(t, key, c.ClusterKey,
			"a definition written with no stamp is recorded against the membership it "+
				"was written over, so it is current until that membership moves")
	}
}
