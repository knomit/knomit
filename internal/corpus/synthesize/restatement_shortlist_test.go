package synthesize

import (
	"context"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/internal/synthtest"
)

// seedShortlist runs the backfill + refresh the way a review session does.
//
// It lives beside the code under test, not in synthtest: the title backfill and
// the shortlist refresh are restatement.go, and synthtest imports only step and
// the store.
func seedShortlist(e *synthtest.RestatementEnv) {
	e.T.Helper()
	ctx := context.Background()
	require.NotNil(e.T, e.Emb, "seedShortlist needs an embedder: without one the axis is empty and have == total == 0 passes")
	// NOT titleBackfillBudget. That is production's 15 s latency budget, and
	// when it expires ensureTitleVectors returns partial coverage with no
	// error. The pair under test has the highest fact ids, so it sits in the
	// last batch and is the first thing a stalled runner drops: the pair never
	// stood, and the test failed as a false claim about the gate under test
	// (knomit#298, CI job 108283236877). An hour cannot expire here; the loop
	// exits as soon as nothing is missing.
	//
	// The coverage assertion is the other half. A partial axis reads as
	// "nothing to find", so the fixture must fail loudly as an incomplete
	// axis rather than let every downstream assertion run on the wrong corpus.
	have, total, err := ensureTitleVectors(ctx, e.Deps(), e.Branch, time.Hour)
	require.NoError(e.T, err)
	require.Equal(e.T, total, have,
		"title backfill incomplete (%d/%d): the fixture's axis must be fully covered, or every downstream assertion is about the wrong corpus",
		have, total)
	_, err = refreshRestatementShortlist(ctx, e.Deps(), e.Branch)
	require.NoError(e.T, err)
}
