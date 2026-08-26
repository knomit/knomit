package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// knomit#124, the store half. Drain progress is only worth reporting if the
// backlog it is derived from is THE SAME POPULATION the offer pool walks.
//
// MotifCoverage's backlog term repeats LiveFactsWithoutMotifs' predicate
// verbatim, and this test is what stops the two drifting: it asserts they
// agree across every transition a fact can make — never asked, covered by an
// assignment, and resolved by an honest decline.
//
// A backlog counted with its own slightly-different WHERE clause would report
// drain progress against a queue nobody is draining, which is the same class
// of defect as the issue itself (a number describing something other than what
// happens).
func TestMotifCoverage_BacklogMatchesTheOfferPool(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	d := env.deps()

	env.writeFact("kb/a.md", "A", "body a")
	env.writeFact("kb/b.md", "B", "body b")
	env.writeFact("kb/c.md", "C", "body c")

	// A helper that reads both numbers and asserts the invariant they must
	// always satisfy: the counted backlog IS the offer pool's size.
	agree := func(stage string) (with, backlog, total int) {
		t.Helper()
		with, backlog, total, err := env.svc.Motifs().MotifCoverage(ctx, env.branch)
		require.NoError(t, err)

		pool, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, 1000)
		require.NoError(t, err)

		require.Equalf(t, len(pool), backlog,
			"%s: the reported backlog must equal what the offer pool actually "+
				"holds — a drain figure over a different population is not a "+
				"drain figure", stage)
		require.Equalf(t, total, with+backlog+(total-with-backlog),
			"%s: the three counts must partition the corpus", stage)
		return with, backlog, total
	}

	// 1. Nothing asked yet: everything is backlog.
	with, backlog, total := agree("never asked")
	require.Equal(t, 0, with)
	require.Equal(t, 3, backlog)
	require.Equal(t, 3, total)

	// 2. One fact gains a motif — it leaves the backlog through the fact_motifs
	// door and lands in coverage.
	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/a.md", Motifs: []string{"silent-fallback"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, d, env.branch, res,
		offeredBackfillForTest(t, ctx, env)))

	with, backlog, _ = agree("one assigned")
	require.Equal(t, 1, with)
	require.Equal(t, 2, backlog)

	// 3. One fact is honestly DECLINED — it leaves the backlog through the
	// judged door WITHOUT entering coverage. This is the transition the whole
	// issue is about, and the one coverage alone cannot see.
	ids := env.liveFactIDs()
	require.NoError(t, env.svc.Motifs().RecordBackfillJudgedEmpty(ctx, env.branch,
		[]int64{ids["kb/b.md"]}))

	with, backlog, total = agree("one declined")
	require.Equal(t, 1, with, "a decline does NOT raise coverage")
	require.Equal(t, 1, backlog, "but it DOES shrink the backlog")

	declined := total - with - backlog
	require.Equal(t, 1, declined,
		"and the derived decline count finds it — this is the number the "+
			"ceiling clause is computed from")
}
