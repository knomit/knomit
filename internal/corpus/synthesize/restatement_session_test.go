package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/restatement"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

// Multi-session dynamics that need the engine: the other TestDynamics_* tests
// moved to the restatement package with the code they drive. These two drive
// whole review sessions (NewReviewerWithOptions), so they stay with the parent.

// TestDynamics_IncrementalSessionsStillEmit is the regression for the defect
// that would have shipped: the budget was scaled by the session's DIRTY SEED
// count, not by corpus size. The first review of a repo full-scans, so seeds ≈
// corpus and everything looked right; every session after it sees a handful of
// changed facts, and a handful times five-per-thousand is zero. The feature
// would have emitted nothing for the entire life of every repo, with a full
// pair cache sitting unread.
func TestDynamics_IncrementalSessionsStillEmit(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	env.WriteFact("kb/restate-a.md", "Cache invalidation on write", "one account of it")
	env.WriteFact("kb/restate-b.md", "Cache invalidation on write", "a different account, at length, of the same thing")
	for i := range 400 {
		env.WriteFact(fmt.Sprintf("kb/filler%d.md", i), fmt.Sprintf("Filler %d", i), fmt.Sprintf("filler body %d", i))
	}

	// Session 1: the full scan. This is the session that always worked.
	first := runSession(env)
	require.NotEmpty(t, first.restatementItems, "the full-scan session emits")

	// Session 2: one fact edited, so the seed pool is a single fact.
	env.WriteFact("kb/filler7.md", "Filler 7 revised", "a revised filler body")
	second := runSession(env)
	require.NotEmpty(t, second.restatementItems,
		"an incremental session must still spend from the CORPUS budget — "+
			"scaling by dirty seeds makes this zero forever after the first review")
	require.Contains(t, strings.Join(second.health, "\n"), "standing restatement pairs")
}

// ── session harness ───────────────────────────────────────────────────────

type sessionOutcome struct {
	sessionID        string
	restatementItems []model.PipelineWorkItem
	health           []string
}

// runSession drives one real review session through the engine and reports what
// the shortlist contributed to it.
func runSession(e *synthtest.RestatementEnv) sessionOutcome {
	e.T.Helper()
	res, err := NewReviewerWithOptions(e.RI, nil, step.EffortNormal, step.ScopeFilter{}).StartSession(context.Background())
	require.NoError(e.T, err)

	out := sessionOutcome{sessionID: res.SessionID, health: res.Health}
	for _, item := range e.WorkItems(res.SessionID) {
		if strings.HasPrefix(item.ClusterKey, restatement.ClusterKeyPrefix) {
			out.restatementItems = append(out.restatementItems, item)
		}
	}
	return out
}
