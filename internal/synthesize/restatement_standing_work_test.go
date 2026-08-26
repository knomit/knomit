package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The corpus-state channel (knomit#155, second half).
//
// The structural sweep drains STANDING pairs — corpus state — but it was
// planned from inside `reviewStrategy.Plan`, which `StartSession` only reaches
// when the dirty set is NON-EMPTY. So the sweep advanced only while the corpus
// kept changing, and never on the caught-up corpus whose backlog it exists to
// drain: the population and the trigger were exact opposites.
//
// This is knomit#115's shape returning with the pass that has it, exactly as
// the rip-out's comment at the empty-seed early return predicted.

// standingEnv is a corpus with standing structural pairs whose review watermark
// has already been advanced past everything — a QUIET corpus, which is the one
// state the old code could not serve.
//
// The watermark is advanced by running a real session to completion rather than
// by writing the watermark row directly: the starvation is a COMPOSITION of the
// seed scan and the completion, and a test that fakes either half cannot see it.
func standingEnv(t *testing.T) (*restatementEnv, *Pipeline) {
	t.Helper()
	ctx := context.Background()
	env := manyTwinsEnvWithFiller(t, structuralAllowance+3, 40)
	env.seedShortlist()

	p := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{}, reviewStrategy{})

	// Drain one full session so the watermark reaches HEAD and the corpus goes
	// quiet. Answers are empty no-ops; nothing here is about what the judge
	// decides, only about the corpus having nothing left to CHANGE.
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	for steps := 0; res.Item != nil && steps < 200; steps++ {
		res, err = p.ContinueSession(ctx, res.SessionID, noopAnswerFor(t, res.Item.Type))
		require.NoError(t, err)
	}
	return env, p
}

func noopAnswerFor(t *testing.T, stepType string) string {
	t.Helper()
	switch stepType {
	case "prune":
		return `{"decisions":[],"merges":[]}`
	case "distill":
		return `{"synthesize":[],"retract":[]}`
	case "reflect":
		return `{"methodologies":[]}`
	}
	t.Fatalf("unexpected step type %q", stepType)
	return ""
}

// TestStanding_QuietCorpusStillDrainsItsStandingPairs is the whole of the
// second half of knomit#155.
//
// A corpus with nothing dirty and a backlog of standing structural pairs must
// still be handed those pairs. Before the hook, StartSession short-circuited to
// completeSession before `Plan` ran, so the standing channel never executed at
// all on exactly this corpus.
func TestStanding_QuietCorpusStillDrainsItsStandingPairs(t *testing.T) {
	ctx := context.Background()
	env, p := standingEnv(t)

	// Fixture assertions FIRST. Both are load-bearing: a corpus that is not
	// actually quiet would reach the ordinary Plan and prove nothing, and a
	// corpus with no standing pairs would give the sweep nothing to find.
	seeds, scan, err := p.dirtyFacts(ctx, env.branch, env.svc.Facts(), env.svc.Search(), env.svc.Pipeline())
	require.NoError(t, err)
	require.Empty(t, seeds,
		"fixture must be QUIET — with a non-empty dirty set this exercises the "+
			"ordinary planned path and says nothing about the early return")
	require.False(t, scan.Scoped, "fixture must be unscoped, or the scope guard short-circuits it")
	standing, err := env.svc.Abstraction().RestatementPairsByMatchKindOldest(ctx, env.branch,
		[]string{store.MatchPathIdentity}, 100_000)
	require.NoError(t, err)
	require.NotEmpty(t, standing, "fixture must hold standing structural pairs")

	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Item,
		"a quiet corpus with standing structural pairs must still be handed work — "+
			"a pass whose trigger is corpus state cannot be planned from an "+
			"edge-triggered seed scan")
	require.Equal(t, "prune", res.Item.Type,
		"the standing channel reaches the judge as an ordinary prune item")
	require.False(t, res.Done, "a session that just planned work is not done")
}

// TestStanding_QuietCorpusSaysWhatItDidWhenItPlansNothing is the legibility
// half, and it is not decoration.
//
// #121's wall was a session returning `done:true` with no health block while
// ~288 facts sat unreachable — "nothing to do" and "the pass that would have
// found something never ran" are indistinguishable from outside. Adding a
// standing channel that can silently find nothing would rebuild that ambiguity
// one layer in.
func TestStanding_QuietCorpusSaysWhatItDidWhenItPlansNothing(t *testing.T) {
	ctx := context.Background()
	// A quiet corpus with NO structural pairs at all: the standing channel runs
	// and legitimately finds nothing.
	env := newRestatementEnv(t, 6)
	p := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{}, reviewStrategy{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	for steps := 0; res.Item != nil && steps < 200; steps++ {
		res, err = p.ContinueSession(ctx, res.SessionID, noopAnswerFor(t, res.Item.Type))
		require.NoError(t, err)
	}

	res, err = p.StartSession(ctx)
	require.NoError(t, err)
	require.True(t, res.Done, "fixture must actually be finished, or this is not the empty path")

	joined := strings.Join(res.Health, "\n")
	require.Contains(t, joined, "nothing has changed since the review watermark",
		"the edge-triggered channel must still explain itself")
	require.Contains(t, joined, "standing work",
		"the standing channel must say it RAN — silence here is #121's wall, "+
			"where 'nothing to do' and 'never ran' read identically")
}

// TestStanding_ScopedQuietSessionDoesNotDrainTheWholeCorpus reinstates
// knomit#128 review MEDIUM-2, on the pass that brought the hazard back.
//
// The standing pools are corpus-wide by construction — and the structural route
// deliberately ignores the scope filter even when one is set — so consulting
// them from a scoped session turns a scope that matched nothing into a
// whole-corpus session nobody asked for. Measured before the original guard: a
// scoped run matching no facts planned work over three facts, all outside the
// scope, and printed no scoped sentence at all.
func TestStanding_ScopedQuietSessionDoesNotDrainTheWholeCorpus(t *testing.T) {
	ctx := context.Background()
	env, _ := standingEnv(t)

	// Fixture assertion: an UNSCOPED pipeline on this same corpus DOES plan
	// standing work. Without this the test below passes against a corpus where
	// the standing channel finds nothing for reasons unrelated to the scope.
	unscoped := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{}, reviewStrategy{})
	open, err := unscoped.StartSession(ctx)
	require.NoError(t, err)
	require.NotNil(t, open.Item,
		"fixture must plan standing work when UNSCOPED, or the scope guard is "+
			"being credited for an empty corpus")

	scoped := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal,
		ScopeFilter{Domain: []string{"a-domain-no-fact-here-carries"}}, reviewStrategy{})
	res, err := scoped.StartSession(ctx)
	require.NoError(t, err)
	require.Nil(t, res.Item,
		"a SCOPED session whose scope matched nothing must not drain the whole "+
			"corpus's standing backlog — a scope must never silently widen")
	require.True(t, res.Done)
	require.Contains(t, strings.Join(res.Health, "\n"), "SCOPED",
		"and it must SAY why it did not, or the guard is indistinguishable from an empty corpus")
}

// TestStanding_HypothesizeStrategyIsUnaffected pins the optionality.
//
// The hook is an interface a strategy may implement. A strategy that does not
// must behave exactly as it did before the hook existed — the engine must not
// have acquired a new failure mode for every tool that has no standing channel.
func TestStanding_HypothesizeStrategyIsUnaffected(t *testing.T) {
	var s any = hypothesizeStrategy{}
	_, implements := s.(standingWorkStrategy)
	require.False(t, implements,
		"hypothesize owns no corpus-state channel; if it grows one, this test "+
			"should be replaced by one that exercises it, not deleted")
}

// TestStanding_PlanStandingWorkPlansOnlyTheStandingChannel pins the ruling that
// the quiet path calls the shortlist DIRECTLY rather than falling through into
// the full edge-triggered Plan.
//
// IT RUNS AT EFFORT MEDIUM, and that is the whole reason it can tell the two
// apart. At EffortNormal with a nil seed pool the two are behaviourally
// IDENTICAL — ScopedCluster returns nothing, there are no prune clusters, the
// distill block needs len(seeds) > 1, and bridge seeding is off — so a test
// written at normal effort passes whichever is called. It was written that way
// first, and the sabotage that swapped in the full Plan walked straight through
// it (S12). At medium, Plan additionally runs the motif vocabulary passes:
// RebuildAliases, planMotifAliasWork, planMotifDefineWork. Those are
// edge-triggered maintenance that a quiet standing sweep has no business
// firing, and they are the observable difference.
func TestStanding_PlanStandingWorkPlansOnlyTheStandingChannel(t *testing.T) {
	ctx := context.Background()
	env, _ := standingEnv(t)

	d := env.deps()
	d.Effort = EffortMedium
	// Fixture assertion: the effort dial must actually be the one that opens the
	// vocabulary block, or this test is back to the indistinguishable case.
	require.True(t, d.Effort.MaintainsVocabulary(),
		"fixture must run at an effort where Plan does MORE than the shortlist, "+
			"or the two paths cannot be told apart")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch, "")
	require.NoError(t, err)
	planned, health, err := reviewStrategy{}.PlanStandingWork(ctx, d, sess, env.branch)
	require.NoError(t, err)
	require.True(t, planned, "fixture must plan something, or the census below is over an empty queue")
	require.NotEmpty(t, health, "the standing channel always reports, planned or not")

	// The DISCRIMINATING assertion, and the one S12 was caught by. The
	// vocabulary passes enqueue an item only when they find judge pairs, but
	// they record a health line UNCONDITIONALLY — so the health block, not the
	// queue, is where "did the full edge-triggered Plan run here?" is actually
	// answerable on a quiet corpus.
	require.NotContains(t, strings.Join(health, "\n"), "motif aliases:",
		"the standing channel must NOT run the motif vocabulary passes — they are "+
			"edge-triggered maintenance, and their health line is the tell that "+
			"the full Plan was called instead of the shortlist")

	items, err := env.svc.Pipeline().PendingPipelineWorkItems(ctx, sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	for _, it := range items {
		require.Equal(t, "prune", it.StepType,
			"the standing channel enqueues shortlist prune items and NOTHING else — "+
				"no distill chunks, no discover items, no motif vocabulary work")
		require.True(t, strings.HasPrefix(it.ClusterKey, restatementClusterKeyPrefix),
			fmt.Sprintf("every item must come from the shortlist; got cluster key %q", it.ClusterKey))
	}
}

// TestStanding_QuietSessionDoesNotRunTheVocabularyPasses is the same ruling
// stated as the negative, on the engine path rather than the strategy method.
//
// A quiet corpus at medium effort must not acquire the motif vocabulary work
// that only an edge-triggered plan owns. Asserting it here as well as above is
// not duplication: the test above calls PlanStandingWork directly, which proves
// the method is right and says nothing about what StartSession calls.
func TestStanding_QuietSessionDoesNotRunTheVocabularyPasses(t *testing.T) {
	ctx := context.Background()
	env, _ := standingEnv(t)

	p := NewPipeline(env.ri, func(ProgressEvent) {}, EffortMedium, ScopeFilter{}, reviewStrategy{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotNil(t, res.Item, "fixture must plan standing work, or there is nothing to census")

	items, err := env.svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	for _, it := range items {
		require.Equal(t, "prune", it.StepType,
			"a quiet session runs the STANDING channel only; motif_alias / "+
				"motif_define are edge-triggered maintenance and must not fire here")
	}
}
