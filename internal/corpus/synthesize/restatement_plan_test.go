package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/restatement"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

// TestPlan_EmitsRestatementItemsAsOrdinaryPruneItems — shape parity with
// cluster items is what lets the existing prompt, schema, and apply path serve
// these unchanged.
func TestPlan_EmitsRestatementItemsAsOrdinaryPruneItems(t *testing.T) {
	ctx := context.Background()
	env := synthtest.NewRestatementEnv(t, 0)
	// Two facts with near-identical titles and unrelated bodies, plus filler so
	// the corpus is large enough to fund a slot.
	env.WriteFact("kb/restate-a.md", "Cache invalidation on write", "one account of it")
	env.WriteFact("kb/restate-b.md", "Cache invalidation on write", "a different account, at length, of the same thing")
	for i := range 400 {
		env.WriteFact(fmt.Sprintf("kb/filler%d.md", i), fmt.Sprintf("Filler %d", i), fmt.Sprintf("filler body %d", i))
	}

	res, err := NewReviewerWithOptions(env.RI, nil, step.EffortNormal, step.ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items := env.WorkItems(res.SessionID)
	var emitted []model.PipelineWorkItem
	for _, it := range items {
		if strings.HasPrefix(it.ClusterKey, restatement.ClusterKeyPrefix) {
			emitted = append(emitted, it)
		}
	}
	require.NotEmpty(t, emitted, "the shortlist put something in front of the judge")
	for _, it := range emitted {
		require.Equal(t, "prune", it.StepType, "a restatement item IS a prune item")
		var facts []step.FactForLLM
		require.NoError(t, json.Unmarshal([]byte(it.FactsJSON), &facts))
		require.Len(t, facts, 2, "exactly the pair")
		require.NotEmpty(t, facts[0].Body, "with the bodies the judge needs")
	}
}

// TestPlan_HealthLinesReachTheCaller — a partial axis or a defunded shortlist
// has to be visible, or "no candidates" reads as "nothing to find".
func TestPlan_HealthLinesReachTheCaller(t *testing.T) {
	ctx := context.Background()
	env := synthtest.NewRestatementEnv(t, 30)

	res, err := NewReviewerWithOptions(env.RI, nil, step.EffortNormal, step.ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)

	joined := strings.Join(res.Health, "\n")
	require.Contains(t, joined, "abstraction coverage")
	require.Contains(t, joined, "standing restatement pairs")
	require.Contains(t, joined, "operating point")
	require.Contains(t, joined, "restatement candidates emitted")
	require.Contains(t, joined, "shortlist throttle")
}
