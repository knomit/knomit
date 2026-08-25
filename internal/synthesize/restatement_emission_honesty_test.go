package synthesize

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#117(a). The emission loop counted candidates and THEN inserted them,
// with `if len(facts) != 2 { continue }` between the two — so any candidate
// whose pair failed to load was dropped with no health line, no warning, and no
// trace anywhere. Measured on core: `restatement candidates emitted: 8`, and
// ZERO restate- items in a fully-drained 48-item queue.
//
// The number was not wrong about what it counted. It counted SELECTION and was
// read as SERVICE, and nothing in the output could tell the two apart.
//
// A pair naming a path that does not resolve is the exact failure: GetByPath
// returns nil, the loop breaks with one fact, and the candidate evaporates.
func TestEnqueueRestatementItems_DropsAreCountedAndReported(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 3)
	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)

	ids := env.factIDs()
	pairs := []store.RestatementPair{
		// Servable: both halves resolve.
		{AFactID: ids["kb/f0.md"], BFactID: ids["kb/f1.md"], APath: "kb/f0.md", BPath: "kb/f1.md"},
		// Not servable: kb/gone.md does not exist. This is the silent drop.
		{AFactID: ids["kb/f2.md"], BFactID: 999999, APath: "kb/f2.md", BPath: "kb/gone.md"},
	}

	served, dropped, err := enqueueRestatementItems(ctx, d, sess, env.branch, pairs)
	require.NoError(t, err)

	require.Equal(t, 1, served,
		"served counts what actually reached the judge, not what was selected")
	require.Equal(t, 1, dropped,
		"the unservable candidate must be COUNTED, not silently skipped")

	// And the queue agrees with the number: exactly one restate- item exists.
	items := env.workItems(sess.ID)
	var restate int
	for _, it := range items {
		if strings.HasPrefix(it.ClusterKey, restatementClusterKeyPrefix) {
			restate++
		}
	}
	require.Equal(t, served, restate,
		"the served count and the queue must not be able to disagree — "+
			"8-counted/0-served is exactly the bug")
}

// The health block is where an operator reads this, so the count it prints must
// be the SERVED count. A drop that changes no visible number is the same bug
// wearing a return value.
func TestRestatementHealth_EmittedIsServedAndDropsAreNamed(t *testing.T) {
	t.Run("drops are stated", func(t *testing.T) {
		lines := strings.Join(healthLines(restatementHealth{Emitted: 1, Dropped: 2}), "\n")
		require.Contains(t, lines, "restatement candidates emitted: 1",
			"the emitted number is what reached the judge")
		require.Contains(t, lines, "2",
			"the dropped count appears somewhere in the block")
		require.Contains(t, strings.ToLower(lines), "drop",
			"a drop must be NAMED, not left for a reader to infer from arithmetic")
	})

	// The common case must not gain a noise line. A health block that always
	// mentions drops trains a reader to skip the line that matters.
	t.Run("no drop line when nothing dropped", func(t *testing.T) {
		lines := strings.Join(healthLines(restatementHealth{Emitted: 3, Dropped: 0}), "\n")
		require.Contains(t, lines, "restatement candidates emitted: 3")
		require.NotContains(t, strings.ToLower(lines), "drop",
			"no drop line when there were no drops")
	})
}

// THE WIRING, THROUGH THE REAL PATH — and this test exists in this form
// because an earlier version of it did not work.
//
// health.Emitted is assigned by SELECTION (selectRestatementCandidates); the
// drop happens later, at ENQUEUE. So a fix that corrects enqueue's return value
// but never feeds it back leaves the number an operator reads exactly as wrong
// as before. The first draft of this test called applyEmissionOutcome itself —
// which asserts that the helper works, not that anything CALLS it. Under a
// sabotage that deleted the call site, it passed.
//
// So: drive planRestatementShortlist, and read the health block it actually
// produced. The invariant asserted is the one that failed on core — the number
// printed equals the number of restate- items in the queue.
func TestPlanRestatementShortlist_HealthEmittedMatchesTheQueue(t *testing.T) {
	ctx := context.Background()
	// 200 facts: shortlistBudget is corpus-scaled (5 per 1000), so a small env
	// gets a budget of ZERO and selects nothing — the test would then pass
	// vacuously with 0 emitted and 0 queued.
	env := newRestatementEnv(t, 200)
	env.seedShortlist()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)

	// A standing pair naming a path that does not resolve, ranked at the top of
	// the distribution so selection cannot help but pick it. This is core's
	// situation: a computed candidate whose half is gone by the time the queue
	// is built.
	ids := env.factIDs()
	require.NoError(t, env.svc.Abstraction().ReplaceRestatementPairs(ctx, env.branch, nil,
		[]store.RestatementPair{{
			APath: "kb/f0.md", BPath: "kb/gone.md",
			AFactID: ids["kb/f0.md"], BFactID: 999999,
			TitleCos: 0.999,
		}}, nil))

	require.NoError(t, planRestatementShortlist(ctx, env.deps(), sess, env.branch, nil))

	// What the operator reads.
	var emitted string
	for _, line := range sess.Health {
		if strings.HasPrefix(line, "restatement candidates emitted:") {
			emitted = line
		}
	}
	require.NotEmpty(t, emitted, "the health block must carry the emitted line")

	// What is actually in the queue.
	var restate int
	for _, it := range env.workItems(sess.ID) {
		if strings.HasPrefix(it.ClusterKey, restatementClusterKeyPrefix) {
			restate++
		}
	}

	require.Contains(t, emitted, "emitted: "+itoa(restate),
		"the emitted number must equal the restate- items actually queued; "+
			"core printed 8 beside a queue containing 0")
	require.Contains(t, strings.ToLower(emitted), "drop",
		"and the unservable candidate must be named, not absorbed into arithmetic")
}

func itoa(n int) string { return strconv.Itoa(n) }
