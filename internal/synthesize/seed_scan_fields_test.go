package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// knomit#122 fix (b). The seed-scan log line carried `seeds` and `elapsed` and
// nothing else, so when knomit-kb walled (#121) the three facts that actually
// explained it — was the call scoped, which path did the scan take, and what
// was the watermark — had to be reconstructed: the scoped flag from a DB
// column, the path from wall-clock timing (sub-millisecond ⇒ empty diff,
// 15–172 ms ⇒ full scan), and the watermark by counting commit-log entries
// against candidate cutoffs until one matched the seed count exactly.
//
// All three are known inside dirtyFacts at the moment it decides. These tests
// pin them to the decision rather than to the log format, so the values stay
// correct even if the line is later reworded.
func TestSeedScan_ReportsPathScopeAndWatermark(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	// No watermark: the first run is a full scan by definition, and there is no
	// watermark to report.
	t.Run("first run is a full scan with no watermark", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/a.md", fact.Epistemic, fact.Observation)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		_, scan, err := r.p.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		require.Equal(t, seedScanFull, scan.Path)
		require.False(t, scan.Scoped)
		require.Empty(t, scan.Watermark, "there is no watermark on a first run")
	})

	// A watermark and no scope: the incremental path, and the watermark it
	// diffed against is reported. This is the combination that produced #121's
	// walls, and the hash is what would have named it.
	t.Run("watermark without scope takes the incremental path", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/base.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))
		writeKindFact(t, svc, branch, "kb/technology/b.md", fact.Epistemic, fact.Observation)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		_, scan, err := r.p.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		require.Equal(t, seedScanIncremental, scan.Path)
		require.False(t, scan.Scoped)
		require.Equal(t, head, scan.Watermark,
			"the incremental path reports the watermark it diffed against")
	})

	// A scope forces the full scan even WITH a watermark (the scoped exemption).
	// Reporting both the scope flag and the path is what makes the exemption
	// visible in the log instead of inferable from timing: #121's diagnosis
	// turned on exactly this pair disagreeing with the operator's belief.
	t.Run("scope forces a full scan even with a watermark", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/base.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))

		r.p.scope = ScopeFilter{Domain: []string{"technology"}}

		gs, idx, pipelineIdx, _ := r.storeIndices()
		_, scan, err := r.p.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		require.Equal(t, seedScanFull, scan.Path,
			"a scoped run is exempt from the watermark on the read side")
		require.True(t, scan.Scoped)
		require.Equal(t, head, scan.Watermark,
			"the watermark is reported even when it did not gate the scan — "+
				"its VALUE is the diagnostic, not whether it was used")
	})
}
