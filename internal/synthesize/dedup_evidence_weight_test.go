package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// The THIRD merge site, which neither issue #94 nor knomit fact f62378e5 names.
//
// dedupCluster's mechanical merge pools the loser's sources onto the winner
// (fullWinner.Sources) but never touches fullWinner.EvidenceWeight — the string
// does not appear in internal/synthesize/dedup.go at all. So the surviving fact
// carries a weight computed against the evidence it had BEFORE the merge, while
// now resting on strictly more of it. Not an erasure like the learn path: a
// silent staleness, which is harder to see and just as wrong.
//
// evidence_weight is defined as a function of the pooled evidence, so a merge
// that pools evidence must recompute it — the same rule, and the same
// mechanism, ApplyPruneDecisions already uses.

// weightedDedupFact writes a fact carrying an explicit evidence_weight, placed
// at a chosen point on the embedding axis so the pair is a mechanical
// near-duplicate by construction rather than by luck.
func weightedDedupFact(t *testing.T, env *restatementEnv, path, title, body string, conf float64, sources int, weight float64) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = fact.Observation
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = conf
	f.Sources = sources
	f.EvidenceWeight = weight
	rendered, err := fact.SerializeFact(f)
	require.NoError(t, err)
	require.Contains(t, rendered, "evidence_weight:",
		"fixture: the fact must actually be written with a weight")
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, f.Path(), rendered,
		"write "+path, "test")
	require.NoError(t, err)
}

func TestDedupCluster_MergeRecomputesEvidenceWeightFromPooledRefs(t *testing.T) {
	ctx := context.Background()

	// Both facts land on the same point of the axis, so the mechanical gate
	// sees a near-duplicate pair with certainty.
	emb := &restatementEmbedder{vectorFor: func(string) []float32 { return axisVector(0) }}
	env := newRestatementEnvWith(t, 0, emb)

	const winnerPath = "kb/alpha/winner.md"
	const loserPath = "kb/alpha/loser.md"
	// The winner is the higher-confidence fact, so mergeFacts keeps it — and
	// its stored weight is the stale number under test.
	const staleWeight = 0.20
	weightedDedupFact(t, env, winnerPath, "Widget fails closed", "one recording", 0.9, 2, staleWeight)
	weightedDedupFact(t, env, loserPath, "Widget fails closed again", "another recording", 0.5, 3, 0.15)

	cluster := []factForLLM{
		{File: winnerPath, Title: "Widget fails closed", Body: "one recording",
			Type: "observation", Confidence: 0.9, Sources: 2},
		{File: loserPath, Title: "Widget fails closed again", Body: "another recording",
			Type: "observation", Confidence: 0.5, Sources: 3},
	}

	// The two candidate answers, computed BEFORE the merge because the loser is
	// deleted by it. Asserting the merged weight is merely "bigger than the
	// stale one" is too weak — a recompute over the WINNER ALONE also clears
	// that bar, and the first draft of this test passed under exactly that
	// sabotage. What has to be pinned is that the recompute POOLED both facts.
	localID := fact.ID12(env.ri.ID())
	pooledWeight, _, _ := computeTransfer(ctx, env.svc.Facts(), env.branch, localID,
		[]string{winnerPath, loserPath})
	winnerOnlyWeight, _, _ := computeTransfer(ctx, env.svc.Facts(), env.branch, localID,
		[]string{winnerPath})
	require.Greater(t, pooledWeight, winnerOnlyWeight,
		"fixture: pooling must move the weight, or this test cannot tell a pooled "+
			"recompute from a winner-only one")
	require.Greater(t, winnerOnlyWeight, staleWeight,
		"fixture: even the WRONG answer beats the stale value, which is why "+
			"'greater than stale' is not the assertion")

	d := env.deps()
	surviving, err := dedupCluster(ctx, cluster, env.svc.Facts(), env.svc.Search(),
		env.dedupThreshold(), reviewTool, d.OnProgress, env.branch, fact.ID12(env.ri.ID()))
	require.NoError(t, err)

	// PRECONDITION, asserted: the merge actually happened. Without this the
	// weight assertion below would pass on a session where nothing merged.
	require.Len(t, surviving, 1, "fixture: the pair must have merged mechanically")
	require.Equal(t, winnerPath, surviving[0].File, "fixture: the higher-confidence fact must win")
	require.Equal(t, 5, surviving[0].Sources,
		"fixture: the merge must have POOLED the sources (2+3) — that is what makes the "+
			"pre-merge weight stale")

	read, err := env.svc.Facts().ReadFact(ctx, env.branch, winnerPath, nil)
	require.NoError(t, err)
	merged, err := fact.ParseFact(winnerPath, read.Content)
	require.NoError(t, err)

	require.InDelta(t, pooledWeight, merged.EvidenceWeight, 1e-9,
		"the surviving fact rests on BOTH facts' evidence, so its weight must be the "+
			"recompute over the pooled refs — not the winner's stale value (%.4f) and "+
			"not a recompute over the winner alone (%.4f)", staleWeight, winnerOnlyWeight)
}
