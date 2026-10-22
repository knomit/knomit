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

// TestPlan_FloodCorpusIsBoundedByTheBudgetNotSuppressed — the design has no
// density cut: a corpus where title similarity does not discriminate still gets
// a cap-sized batch, and it is that corpus's OWN verdicts that defund it.
func TestPlan_FloodCorpusIsBoundedByTheBudgetNotSuppressed(t *testing.T) {
	ctx := context.Background()
	env := synthtest.NewRestatementEnv(t, 0)
	// Every title identical: on this corpus the axis says everything restates
	// everything.
	for i := range 600 {
		env.WriteFact(fmt.Sprintf("kb/flood%d.md", i), "Same headline every time",
			fmt.Sprintf("body %d, quite different in wording from the others", i))
	}

	res, err := NewReviewerWithOptions(env.RI, nil, step.EffortNormal, step.ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)

	emitted := 0
	for _, it := range env.WorkItems(res.SessionID) {
		if strings.HasPrefix(it.ClusterKey, restatement.ClusterKeyPrefix) {
			emitted++
		}
	}
	require.LessOrEqual(t, emitted, restatement.MaxShortlistItems,
		"bounded by the judge-slot budget, never by a rate anyone hard-coded")
	require.Contains(t, strings.Join(res.Health, "\n"), "standing restatement pairs",
		"and the flood is visible in the health output")
}
