package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// knomit#115. StartSession returns before the strategy plans when the
// watermark-diffed seed pool is empty — and an unscoped session's completion
// advances the watermark past the session's OWN writes. Composed: after one
// full-scan session on a quiet corpus, every subsequent unscoped session
// returns done:true forever.
//
// But motif backfill's pool (LiveFactsWithoutMotifs) is corpus-wide and
// watermark-independent BY DESIGN. It is a LEVEL-triggered pass — the corpus
// state IS the trigger — and it was being starved by a gate that only looks at
// recent commits. The consequence beyond a bulk run: a corpus hydrated from a
// remote (facts present, no new commits) never backfills at all.
//
// newQuietCorpusReviewer builds exactly that situation: facts exist, none carry
// motifs, and the watermark already sits at HEAD so the dirty set is empty.
func newQuietCorpusReviewer(t *testing.T, effort Effort) (*Reviewer, *store.Service, string) {
	t.Helper()
	const branch = "agent/test"
	ctx := context.Background()

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	// Several epistemic facts, none carrying a motif — the backfill pool.
	for _, p := range []string{"kb/technology/a.md", "kb/technology/b.md", "kb/technology/c.md"} {
		writeKindFact(t, svc, branch, p, fact.Epistemic, fact.Observation)
	}

	// The watermark at HEAD is what makes the dirty set empty. This is the
	// state an unscoped session leaves behind on a quiet corpus, and the state
	// a freshly-hydrated remote clone starts in.
	head, err := svc.Branches().HeadCommit(ctx, branch)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return NewReviewerWithOptions(ri, nil, effort, ScopeFilter{}), svc, branch
}

// The fix: an empty dirty set must not hide level-triggered work. At an effort
// that maintains vocabulary, a corpus with un-motifed facts has work to do, and
// the session must plan it rather than reporting success at having done
// nothing.
func TestStartSession_EmptyDirtySetStillPlansLevelTriggeredWork(t *testing.T) {
	r, svc, branch := newQuietCorpusReviewer(t, EffortMedium)
	ctx := context.Background()

	// Precondition: the dirty set really is empty, so this test is exercising
	// the early-exit path and not merely a corpus that happened to have seeds.
	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, scan, err := r.p.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Empty(t, seeds, "precondition: the watermark-diffed seed pool is empty")
	require.Equal(t, seedScanIncremental, scan.Path)

	// Precondition: there IS level-triggered work — facts without motifs.
	targets, err := svc.Motifs().LiveFactsWithoutMotifs(ctx, branch, 100)
	require.NoError(t, err)
	require.NotEmpty(t, targets, "precondition: the backfill pool is non-empty")

	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.False(t, res.Done,
		"a corpus with pending level-triggered work must not report done — "+
			"LiveFactsWithoutMotifs is watermark-independent by design, and the "+
			"corpus state IS the trigger")
}

// MN5's guarantee, protected explicitly. The effort gate still governs: at
// normal effort the vocabulary passes do not run, so there is no
// level-triggered work to rescue and the session completes exactly as before.
//
// This is the trap in this fix, and it is not hypothetical. Backfill fires on
// any authored fact LACKING a motif — which is every fact on a motif-free
// corpus, precisely the corpus MN5's test uses. A level-triggered check placed
// OUTSIDE the effort gate would make a normal-effort session start planning
// backfill work, changing what EffortNormal produces
// (invariants/synthesize/motif/effort-amendment).
// Asserting only `res.Done` here would NOT catch the bug this test is for.
// Plan gates the vocabulary passes a second time, so a level-triggered check
// with its effort gate removed still ends with a completed session at normal
// effort — done:true either way. (Measured: under exactly that sabotage this
// test passed while the health-line test failed.) So the assertions below name
// the two things that actually differ.
func TestStartSession_EmptyDirtySetAtNormalEffortStillCompletes(t *testing.T) {
	r, svc, branch := newQuietCorpusReviewer(t, EffortNormal)
	ctx := context.Background()

	// Same corpus as the test above: the backfill pool is non-empty. The ONLY
	// difference is the effort dial.
	targets, err := svc.Motifs().LiveFactsWithoutMotifs(ctx, branch, 100)
	require.NoError(t, err)
	require.NotEmpty(t, targets, "precondition: same corpus, backfill pool non-empty")

	// 1. The predicate itself, directly. This is the assertion that fails the
	// moment the effort gate leaves HasLevelTriggeredWork, wherever it goes.
	d := r.p.deps()
	has, err := reviewStrategy{}.HasLevelTriggeredWork(ctx, d, branch)
	require.NoError(t, err)
	require.False(t, has,
		"EffortNormal must report no level-triggered work even on a motif-free "+
			"corpus: backfill fires on every fact there, which is the corpus MN5's "+
			"test uses (invariants/synthesize/motif/effort-amendment)")

	// 2. The path actually taken. The early-exit path is the one that appends
	// the empty-seed health line; the plan path never does. So the presence of
	// that line is proof the session completed WITHOUT planning — which
	// `res.Done` alone cannot tell you, since both paths end done.
	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotEmpty(t, res.Health,
		"the session must complete via the early-exit path, not by planning "+
			"an empty queue and completing at the end of it")
}

// knomit#122 fix (c), merged here because it is #115's user-visible half. An
// empty return said nothing: done:true, no health block, indistinguishable
// from a finished corpus. That ambiguity is exactly what made #121's wall read
// as completion for hours.
//
// Two distinct empty cases must be distinguishable in the response itself.
func TestStartSession_EmptyReturnSaysWhyItIsEmpty(t *testing.T) {
	ctx := context.Background()

	// Case 1: watermark at HEAD, nothing changed. The line must name the
	// watermark and the remedy, because the facts behind it are reachable only
	// via a scope or a watermark reset.
	t.Run("nothing changed since the watermark", func(t *testing.T) {
		r, svc, branch := newQuietCorpusReviewer(t, EffortNormal)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)

		res, err := r.StartSession(ctx)
		require.NoError(t, err)
		require.True(t, res.Done)

		health := strings.Join(res.Health, "\n")
		require.NotEmpty(t, res.Health,
			"an empty return with no health block cannot be told from a finished corpus")
		require.Contains(t, health, "watermark",
			"the line names the mechanism that made the pool empty")
		require.Contains(t, health, head[:8],
			"the line carries the watermark hash — the value that had to be "+
				"reconstructed by arithmetic in #121")
	})

	// Case 2: a scope that matches nothing. Same done:true shape, different
	// cause, and the operator needs to tell them apart — this is the pair the
	// #115 comment measured as ambiguous exactly when it mattered.
	t.Run("scope matched no facts", func(t *testing.T) {
		r, _, _ := newQuietCorpusReviewer(t, EffortNormal)
		r.p.scope = ScopeFilter{Entities: []string{"nothing-has-this-entity"}}

		res, err := r.StartSession(ctx)
		require.NoError(t, err)
		require.True(t, res.Done)

		health := strings.Join(res.Health, "\n")
		require.NotEmpty(t, res.Health)
		require.Contains(t, health, "scope",
			"a scoped empty return must say the SCOPE matched nothing, not blame the watermark")
		require.NotContains(t, health, "watermark",
			"a scoped run is exempt from the watermark on both halves — "+
				"naming it here would send the reader to the wrong mechanism")
	})
}
