package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The pair used throughout this file: two facts about one event, filed under
// two different freeform categories, whose stored vectors sit ABOVE the
// model's mechanical dedup floor. That is the population #127 is about —
// measured on the live core corpus, every confirmed duplicate pair had a
// blended cosine at or above the floor (0.83–0.97 against a floor of 0.82)
// while the pairs that DID reach the judge topped out at 0.77.
const (
	certainAPath = "kb/technology/security/vulnerabilities/network-security/alpha.md"
	certainBPath = "kb/technology/security/vulnerabilities/network-appliances/beta.md"
	certainTitle = "SmartConsole authentication bypass under active exploitation"
	certainBody  = "The same event, recorded twice under two freeform categories."
)

// certaintyEnv builds a corpus whose only near-duplicate pair sits above the
// mechanical dedup floor, with n filler facts scattered elsewhere on the axis.
//
// The two twins are placed by hand on the first two dimensions so the test
// controls the cosine exactly; every other fact keeps the default hash vector,
// which is far from both.
func certaintyEnv(t *testing.T, n int) *restatementEnv {
	t.Helper()
	titleB := certainTitle + " (weekly recap)"
	bodyB := certainBody + " Filed a second time."
	emb := &restatementEmbedder{vectorFor: func(text string) []float32 {
		switch text {
		case certainTitle:
			return axisVector(0)
		case titleB:
			return axisVector(0.02)
		case certainTitle + " " + certainBody:
			return axisVector(0.01)
		case titleB + " " + bodyB:
			return axisVector(0.03)
		}
		return nil
	}}
	env := newRestatementEnvWith(t, n, emb)
	env.writeFact(certainAPath, certainTitle, certainBody)
	env.writeFact(certainBPath, titleB, bodyB)
	return env
}

// certainPairIsAboveTheFloor asserts the fixture actually does what it claims.
//
// A fixture chosen to make a test discriminate must be ASSERTED, not assumed
// (the campaign's fixture-vacuity lesson): if these vectors drifted below the
// floor the tests below would pass while measuring nothing.
func certainPairIsAboveTheFloor(t *testing.T, env *restatementEnv) {
	t.Helper()
	ids := env.liveFactIDs()
	vecs, err := env.svc.Abstraction().BodyVectorsByFactID(context.Background(),
		[]int64{ids[certainAPath], ids[certainBPath]})
	require.NoError(t, err)
	cos := store.CosineSim(vecs[ids[certainAPath]], vecs[ids[certainBPath]])
	require.GreaterOrEqual(t, cos, env.dedupThreshold(),
		"fixture must sit at or above the mechanical dedup floor, or these tests measure nothing")
}

// TestShortlist_CertainDuplicatesReachTheStandingCache is the core of #127.
//
// The predicate that identifies a pair as a CERTAIN duplicate — blended cosine
// at or above the model's dedup floor — used to be the predicate that deleted
// it from the shortlist, on the rationale that mergeFacts would handle it.
// mergeFacts only merges WITHIN a cluster, so for a cross-cluster pair nothing
// handled it: certainty was the disqualifier.
func TestShortlist_CertainDuplicatesReachTheStandingCache(t *testing.T) {
	ctx := context.Background()
	env := certaintyEnv(t, 0)
	certainPairIsAboveTheFloor(t, env)

	env.seedShortlist()

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100)
	require.NoError(t, err)
	require.True(t, containsPair(pairs, certainAPath, certainBPath),
		"a duplicate pair above the mechanical floor must stand in the shortlist — "+
			"no cluster-scoped merge will ever reach it")
}

// TestShortlist_CertainDuplicatesAreEnqueuedForTheJudge is the wiring half.
//
// Deliberately drives planRestatementShortlist rather than the helper: a test
// that calls the helper asserts the helper works, not that anything calls it
// (the campaign's path-vs-state rule). It asserts the item actually reaches the
// queue, carrying both halves of the pair.
func TestShortlist_CertainDuplicatesAreEnqueuedForTheJudge(t *testing.T) {
	ctx := context.Background()
	// 200 facts: shortlistBudget is corpus-scaled at 5 per 1000, so a small
	// corpus budgets ZERO and the test would pass vacuously at 0 enqueued.
	env := certaintyEnv(t, 200)
	certainPairIsAboveTheFloor(t, env)

	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
	require.NoError(t, err)
	require.NoError(t, planRestatementShortlist(ctx, d, sess, env.branch, nil))

	require.True(t, shortlistQueueHoldsPair(t, env, sess.ID, certainAPath, certainBPath),
		"the certain duplicate must be enqueued as a shortlist prune item")
}

// TestShortlist_CoClusteredCertainDuplicatesAreNotEnqueued pins the exclusion
// that SURVIVES.
//
// "Prune already sees this pair" is a real exclusion and it is implemented
// once, correctly, at selection time as a cluster co-membership check. This
// test is the reason the fix is "delete the cosine proxy" rather than "delete
// the exclusion": under a change that removed co-membership too, the test
// above still passes and only this one fails.
func TestShortlist_CoClusteredCertainDuplicatesAreNotEnqueued(t *testing.T) {
	ctx := context.Background()
	env := certaintyEnv(t, 200)
	certainPairIsAboveTheFloor(t, env)

	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
	require.NoError(t, err)
	coClustered := [][]factForLLM{{
		{File: certainAPath}, {File: certainBPath},
	}}
	require.NoError(t, planRestatementShortlist(ctx, d, sess, env.branch, coClustered))

	require.False(t, shortlistQueueHoldsPair(t, env, sess.ID, certainAPath, certainBPath),
		"a pair prune already sees in one cluster must not also spend a shortlist slot")
}

// shortlistQueueHoldsPair reports whether a shortlist-originated work item in
// this session carries exactly the two named paths.
func shortlistQueueHoldsPair(t *testing.T, env *restatementEnv, sessionID, a, b string) bool {
	t.Helper()
	ctx := context.Background()
	for {
		item, err := env.svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
		require.NoError(t, err)
		if item == nil {
			return false
		}
		if isShortlistItem(item.ClusterKey) && itemCarriesPaths(t, item.FactsJSON, a, b) {
			return true
		}
		// Consume it so the next peek advances; the queue is a priority queue,
		// not a cursor.
		ok, err := env.svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, "{}")
		require.NoError(t, err)
		require.True(t, ok)
	}
}

func isShortlistItem(clusterKey string) bool {
	return len(clusterKey) > len(restatementClusterKeyPrefix) &&
		clusterKey[:len(restatementClusterKeyPrefix)] == restatementClusterKeyPrefix
}

func itemCarriesPaths(t *testing.T, factsJSON string, want ...string) bool {
	t.Helper()
	var facts []factForLLM
	require.NoError(t, json.Unmarshal([]byte(factsJSON), &facts))
	if len(facts) != len(want) {
		return false
	}
	have := map[string]struct{}{}
	for _, f := range facts {
		have[f.File] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}

var _ = fmt.Sprintf
