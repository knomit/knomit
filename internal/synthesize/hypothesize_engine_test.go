package synthesize

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Engine-level tests for the hypothesize pipeline. Before the Strategy
// extraction, hypothesize had its own hand-written session loop in
// internal/mcp and no test ever drove that loop end to end — every test poked a
// single helper. These drive real sessions to completion through the shared
// engine, which is where the loop now lives.

// newHypothesizeTestRepo opens a fresh store on branch agent/test and returns
// it with a RepoInstance bound to it. No embedder: every search path exercised
// here is SQL-only.
func newHypothesizeTestRepo(t *testing.T) (*store.Service, *repos.RepoInstance) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return svc, ri
}

// writeTestFact commits one fact of the given type on branch agent/test.
func writeTestFact(t *testing.T, svc *store.Service, path, title string, typ fact.Type, domain ...string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "body of " + title
	f.Type = typ
	// Origin is left unset so serialize/parse apply the type-aware default
	// the production write paths rely on: distilled for synthesis facts,
	// authored otherwise. Hardcoding distilled here paired it with
	// observation types, which is not a fact that can exist.
	f.Confidence = 0.8
	f.Sources = 1
	f.Domain = domain
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// drainHypothesizeSession answers every item with the empty response until the
// session reports done, returning the item types in the order they were served.
// It caps the walk so a queue that never drains fails loudly instead of hanging.
func drainHypothesizeSession(t *testing.T, p *Pipeline, res *PipelineResult) []string {
	t.Helper()
	ctx := context.Background()
	var seen []string
	for steps := 0; res.Item != nil; steps++ {
		require.Less(t, steps, 50, "session did not drain in 50 steps")
		seen = append(seen, res.Item.Type)
		var err error
		res, err = p.ContinueSession(ctx, res.SessionID, "")
		require.NoError(t, err)
	}
	require.True(t, res.Done, "a session with no remaining item must report done")
	return seen
}

// TestHypothesizer_NormalEffort_DefaultPath is the engine-level loop test:
// three synthesis facts produce three "hypothesize" work items, the session
// drains to done, and — this is the load-bearing half — not one "discover" item
// appears at normal effort (invariants/synthesize/effort-normal-byte-identical).
//
// The empty responses also exercise the acknowledgement placeholder: a
// hypothesize item carries no decodable content, so Decode normalizes "" to
// "acknowledged" rather than rejecting it. An item that failed to normalize
// would be re-served forever and the step cap above would trip.
func TestHypothesizer_NormalEffort_DefaultPath(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	for _, slug := range []string{"alpha", "beta", "gamma"} {
		writeTestFact(t, svc, "kb/test/"+slug+".md", slug, fact.Synthesis, "test")
	}

	p := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{})
	require.Equal(t, EffortNormal, p.Effort())

	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)
	require.False(t, res.Done)

	seen := drainHypothesizeSession(t, p, res)
	require.Equal(t, []string{"hypothesize", "hypothesize", "hypothesize"}, seen,
		"one work item per synthesis fact, and nothing else at normal effort")

	// The session must be closed out, not merely out of items.
	sess, err := svc.Pipeline().GetPipelineSession(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, "completed", sess.Status)

	// Unscoped completion advances the watermark so the next run is incremental.
	wm, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)
	require.NotEmpty(t, wm)
}

// TestHypothesizer_SeedsSynthesisOnly_BothScanPaths pins AcceptSeed to both
// halves of the seed scan. The SQL type filter in SeedQuery cannot run on the
// incremental (DiffFiles) path, so the Go predicate is the authoritative one
// and must behave identically on each — otherwise the seed pool would depend on
// whether a watermark happens to exist.
func TestHypothesizer_SeedsSynthesisOnly_BothScanPaths(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	// Full-scan path (no watermark): one synthesis fact among three.
	writeTestFact(t, svc, "kb/test/synth-a.md", "synth-a", fact.Synthesis, "test")
	writeTestFact(t, svc, "kb/test/obs-a.md", "obs-a", fact.Observation, "test")
	writeTestFact(t, svc, "kb/test/hyp-a.md", "hyp-a", fact.Hypothesis, "test")

	p := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"hypothesize"}, drainHypothesizeSession(t, p, res),
		"full scan must seed synthesis facts only")

	// Incremental path (watermark now set by the completion above): same mix.
	writeTestFact(t, svc, "kb/test/synth-b.md", "synth-b", fact.Synthesis, "test")
	writeTestFact(t, svc, "kb/test/obs-b.md", "obs-b", fact.Observation, "test")

	p2 := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{})
	res2, err := p2.StartSession(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"hypothesize"}, drainHypothesizeSession(t, p2, res2),
		"incremental scan must apply the same synthesis-only predicate")
}

// TestHypothesizer_ZeroSeeds_CreatesAndCompletesSession pins D5: a start that
// finds nothing no longer short-circuits before creating a session. It creates
// one, completes it, and reports its id — so `session_id` is populated on the
// done turn where it used to be empty (additive on the wire; the field already
// existed).
func TestHypothesizer_ZeroSeeds_CreatesAndCompletesSession(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	res, err := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotEmpty(t, res.SessionID, "D5: the zero-seed path creates a real session")
	require.Nil(t, res.Item)

	sess, err := svc.Pipeline().GetPipelineSession(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, "completed", sess.Status)
}

// TestHypothesizer_ScopedNonEmptyWatermark_StillSeedsInScope is the regression
// guard for the read-side watermark gating bug, ported from internal/mcp onto
// the engine. A scoped hypothesize run must re-examine its whole scope
// regardless of the shared "hypothesize" watermark: scoped runs don't ADVANCE
// the watermark, so they must not be BLOCKED by it either
// (decisions/architecture/synthesize/scope-filter).
func TestHypothesizer_ScopedNonEmptyWatermark_StillSeedsInScope(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	writeTestFact(t, svc, "kb/arch/a.md", "a", fact.Synthesis, "auth")
	writeTestFact(t, svc, "kb/arch/b.md", "b", fact.Synthesis, "auth")

	// Simulate a prior unscoped run having advanced the watermark to HEAD.
	head, err := svc.Branches().HeadCommit(ctx, "agent/test")
	require.NoError(t, err)
	require.NotEmpty(t, head)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "hypothesize", "agent/test", head))

	p := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{Domain: []string{"auth"}})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.False(t, res.Done,
		"scoped hypothesize must seed its whole scope even when the watermark is at HEAD")
	require.Equal(t, []string{"hypothesize", "hypothesize"}, drainHypothesizeSession(t, p, res))
}

// TestHypothesizer_BackwardDiscovery_SingleFact checks that a high-effort start
// with only one in-scope synthesis fact completes cleanly: the len(seeds) >= 2
// bridge guard skips discovery without erroring, and the single per-fact item
// still flows.
func TestHypothesizer_BackwardDiscovery_SingleFact(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	writeTestFact(t, svc, "kb/arch/only.md", "only", fact.Synthesis, "auth")

	p := NewHypothesizer(ri, nil, EffortHigh, ScopeFilter{Domain: []string{"auth"}})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.False(t, res.Done, "one synthesis fact in scope → session should start")
	require.Equal(t, []string{"hypothesize"}, drainHypothesizeSession(t, p, res),
		"a single seed cannot bridge, so no discover item may appear")
}

// insertManualDiscoverItem queues a backward-shaped discover item on an
// existing session, bypassing the bridge engine. The payload uses the forward
// direction so the backward-only BlastRadius gate does not fire in tests that
// let apply run.
func insertManualDiscoverItem(t *testing.T, svc *store.Service, sessionID string) {
	t.Helper()
	require.NoError(t, svc.Pipeline().InsertPipelineWorkItem(context.Background(), store.PipelineWorkItem{
		SessionID:  sessionID,
		StepType:   "discover",
		ClusterKey: "discover-bwd-0",
		FactsJSON:  `{"direction":"forward","bridge":{"token":"auth","kind":"entity","members":[]}}`,
		Priority:   backwardDiscoverPriority(0),
	}))
}

// TestHypothesizer_DiscoverParseFailure_NonFatal mirrors the forward path's
// TestReviewer_DiscoverParseFailure_NonFatal: a malformed discover *response*
// must be non-fatal. The item is still claimed, no fact is written, and the
// turn returns no error — aborting would kill the session and lose the grounded
// work still queued behind it.
func TestHypothesizer_DiscoverParseFailure_NonFatal(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "hypothesize", "agent/test", "")
	require.NoError(t, err)
	insertManualDiscoverItem(t, svc, sess.ID)

	p := NewHypothesizer(ri, nil, EffortHigh, ScopeFilter{})
	_, err = p.ContinueSession(ctx, sess.ID, `not json {{{`)
	require.NoError(t, err, "a malformed discover response must be non-fatal")

	results, err := svc.Search().Search(ctx, "agent/test", store.SearchOptions{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, results, "no fact must be written when the discover response is unparseable")

	next, err := svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.Nil(t, next, "the discover item must be marked answered, leaving no unanswered work")
}

// discoverResponseOneProposal is a valid forward-discover response proposing
// exactly one fact. With empty bridge members, refs=[] satisfies the
// refs-cover-seeds check and confidence 0.9 clears the 0.5 gate.
const discoverResponseOneProposal = `{"proposals":[{"path":"kb/x/p.md","title":"P","body":"B","type":"synthesis","domain":["auth"],"confidence":0.9,"entities":[],"refs":[]}]}`

// TestHypothesizer_ConcurrentDiscoverSubmission_WritesOnce is the hypothesize
// half of the P0.4 claim anchor (review_claim_test.go covers the review half):
// two callers answering the same discover item must not both write its
// proposals. The submissions are concurrent because that is the shape in which
// the bug is reachable — any pair of callers that both peek before either
// records its answer.
//
// This replaces internal/mcp's TestHypothesizeContinue_MarkAnsweredBeforeApply,
// which asserted the same property indirectly by faulting the claim through a
// mocked PipelineIndex. The claim protocol is engine-owned now, so the property
// is asserted directly against a real store: one item, two submissions, exactly
// one fact on disk. Note the swap is not like-for-like — the deleted test also
// pinned the claim's error branch, which nothing covers now (see the UNCOVERED
// note at the claim site in pipeline.go).
func TestHypothesizer_ConcurrentDiscoverSubmission_WritesOnce(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "hypothesize", "agent/test", "")
	require.NoError(t, err)
	insertManualDiscoverItem(t, svc, sess.ID)

	p := NewHypothesizer(ri, nil, EffortHigh, ScopeFilter{})
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(2)
	for range 2 {
		go func() {
			defer done.Done()
			start.Wait()
			// Errors are not asserted: whichever caller arrives after the
			// session completes gets a "not active" error, which is benign. The
			// assertion that matters is the fact count below.
			_, _ = p.ContinueSession(ctx, sess.ID, discoverResponseOneProposal)
		}()
	}
	start.Done()
	done.Wait()

	results, err := svc.Search().Search(ctx, "agent/test", store.SearchOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, results, 1, "a duplicated discover submission must not duplicate its proposals")
}

// TestBackwardDiscoverPriority_StrictlyNegativeRanked is the regression guard
// for the backward priority-leak bug, mirroring the forward path's
// TestForwardDiscoverPriority_StrictlyNegativeRanked.
//
// Backward "discover" work items must run AFTER the per-fact hypothesize items,
// which carry positive priorities, so their priority must stay strictly negative
// regardless of how large a member's BlastRadius is. The old formula fed blast
// straight into the priority (`-100 + blast`); a high-blast keystone then
// produced a positive priority that leapfrogged the per-fact band. The fix ranks
// by position in the BlastRadius-sorted slice, so priority is a function of rank
// only — backwardDiscoverPriority takes no blast argument at all, so blast
// magnitude cannot leak in by construction.
func TestBackwardDiscoverPriority_StrictlyNegativeRanked(t *testing.T) {
	// Even a very large discover queue stays strictly below the per-fact band (0).
	prev := 0.0
	for rank := 0; rank < 1000; rank++ {
		p := backwardDiscoverPriority(rank)
		if p >= 0 {
			t.Fatalf("backwardDiscoverPriority(%d) = %v, must be strictly negative (below the per-fact band)", rank, p)
		}
		if rank > 0 && p >= prev {
			t.Fatalf("priority must strictly decrease with rank: rank %d = %v, prev = %v", rank, p, prev)
		}
		prev = p
	}

	// Highest-blast keystone (rank 0) sits at the top of the discover band.
	if got := backwardDiscoverPriority(0); got != backwardDiscoverPriorityBase {
		t.Errorf("rank 0 priority = %v, want %v (top of discover band)", got, backwardDiscoverPriorityBase)
	}

	// The base must be strictly negative so discover never outranks the
	// positive-priority per-fact hypothesize items.
	if backwardDiscoverPriorityBase >= 0 {
		t.Errorf("discover band base %v must be strictly negative", backwardDiscoverPriorityBase)
	}
}

// TestHypothesizeStrategy_PerFactPriorityStaysPositive pins the other half of
// that band separation: the per-fact items' priorities must all be > 0, which
// is what makes "strictly negative" a meaningful floor for the discover band.
func TestHypothesizeStrategy_PerFactPriorityStaysPositive(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()

	for _, slug := range []string{"a", "b", "c"} {
		writeTestFact(t, svc, "kb/test/"+slug+".md", slug, fact.Synthesis, "test")
	}

	res, err := NewHypothesizer(ri, nil, EffortNormal, ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)

	// Walk the queue through the index rather than the engine, so the stored
	// priority — not just the serving order — is what gets asserted.
	seen := 0
	for {
		it, err := svc.Pipeline().NextPipelineWorkItem(ctx, res.SessionID)
		require.NoError(t, err)
		if it == nil {
			break
		}
		require.Equal(t, "hypothesize", it.StepType)
		require.Greater(t, it.Priority, 0.0,
			"per-fact items must stay above the discover band's zero floor")
		claimed, err := svc.Pipeline().AnswerPipelineWorkItem(ctx, it.ID, "ack")
		require.NoError(t, err)
		require.True(t, claimed)
		seen++
	}
	require.Equal(t, 3, seen)
}
