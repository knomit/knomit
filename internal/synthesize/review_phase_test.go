package synthesize

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Tests for the phase-state-machine dispatcher in Reviewer. The pre-fix code
// kept "have we considered enqueueing reflect for this session?" on an
// in-memory map (`reflectChecked`) on the Reviewer struct. Because the MCP
// handler constructs a fresh Reviewer per call, that map was empty on every
// continuation, so reflect items were re-enqueued indefinitely on sessions
// with hypothesis transitions. The fix moves the state to the
// `pipeline_sessions.phase` column with a CAS-guarded transition.

// TestReviewer_PhaseAdvances_WorkToReflectToDone is the happy-path test for
// a session that has both work items and hypothesis transitions. It pins
// the externally observable contract:
//
//   - phase=work after the session is started
//   - phase=reflect once the last prune/distill item is answered (and
//     transitions exist)
//   - phase=done once the reflect item is answered
//   - the final ContinueSession returns Done:true with Remaining:0
func TestReviewer_PhaseAdvances_WorkToReflectToDone(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	// Set up a hypothesis transition: write a hypothesis fact, mark the
	// commit as the prior watermark, then modify the fact so its type is
	// no longer hypothesis. findHypothesisTransitions will see one
	// "promoted" transition between watermark and HEAD.
	seedHypothesisTransition(t, svc, branch)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	// Phase starts at "work".
	got, err := svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "work", got.Phase)

	// Answer the prune item with an empty no-op response. Reviewer must
	// detect the empty queue, advance phase work->reflect, and return the
	// freshly-enqueued reflect item.
	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.NotNil(t, res.Item, "reflect item should be served")
	require.Equal(t, "reflect", res.Item.Type)

	got, err = svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "reflect", got.Phase, "phase must advance to reflect after last work item")

	// Answer reflect (informational; the body is irrelevant for this test —
	// the response shape will be tightened by a separate spec).
	res, err = r.ContinueSession(ctx, sess.ID, `{"reasoning":"none","reinforce":[],"propose":[]}`)
	require.NoError(t, err)
	require.True(t, res.Done, "session must complete after reflect is answered")
	require.NotNil(t, res.Progress)
	require.Equal(t, 0, res.Progress.Remaining, "remaining must hit zero on completion")

	got, err = svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.Phase)
	require.Equal(t, "completed", got.Status)
}

// TestReviewer_NoTransitions_SkipsReflect verifies the no-transitions path:
// the session advances work -> reflect -> done in a single ContinueSession
// call after the last work item is answered, and no reflect work item is
// ever inserted.
func TestReviewer_NoTransitions_SkipsReflect(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	// No watermark set → findHypothesisTransitions short-circuits and
	// returns empty. The reflect phase must immediately become done.
	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.True(t, res.Done, "with no transitions, session should complete immediately after work")
	require.Nil(t, res.Item)

	got, err := svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.Phase)
	require.Equal(t, "completed", got.Status)

	// No reflect work item inserted: total work items == 1 (the prune we
	// inserted manually).
	completed, remaining, err := svc.Pipeline().PipelineWorkItemStats(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, completed)
	require.Equal(t, 0, remaining)
}

// TestReviewer_StatelessAcrossInstances is the load-bearing regression for
// this bug. Pre-fix, the second Reviewer instance had an empty
// `reflectChecked` map and would enqueue another reflect item after the
// first one was answered, returning a *new* reflect prompt instead of
// completing the session. The test runs prune through one Reviewer and
// reflect through a freshly-constructed second Reviewer, then asserts the
// session reaches Done.
func TestReviewer_StatelessAcrossInstances(t *testing.T) {
	r1, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	seedHypothesisTransition(t, svc, branch)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	// First Reviewer: answer the prune item; expect the reflect prompt.
	res, err := r1.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.NotNil(t, res.Item)
	require.Equal(t, "reflect", res.Item.Type)

	// Second Reviewer: a fresh instance with an empty in-memory state.
	// Under the bug, this caller would re-enqueue a reflect item and never
	// reach Done. With the fix, the persistent phase column (set by the
	// first Reviewer's transition) prevents re-enqueue.
	r2 := NewReviewer(r1.ri, nil)
	res, err = r2.ContinueSession(ctx, sess.ID, `{"reasoning":"none","reinforce":[],"propose":[]}`)
	require.NoError(t, err)
	require.True(t, res.Done, "fresh Reviewer must still drive the session to completion — "+
		"reflect-already-considered state lives on the session row, not the Reviewer instance")

	// Exactly two work items in the session: one prune + one reflect.
	// More than that means the second Reviewer re-enqueued reflect — the bug.
	completed, remaining, err := svc.Pipeline().PipelineWorkItemStats(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, 2, completed, "expected exactly one prune + one reflect, both answered")
	require.Equal(t, 0, remaining, "no work items should remain unanswered")
}

// TestReviewer_ConcurrentContinuations_EnqueueReflectOnce verifies the CAS
// guarantee: two callers racing on the dispatcher when phase=work and no
// unanswered work items remain must not both enqueue a reflect work item.
// Pre-fix (in-memory `reflectChecked` map per Reviewer call) would have
// *both* callers fall into the enqueue branch, doubling the queue. Post-fix
// the work→reflect CAS lets exactly one caller insert.
//
// We drive nextItem directly instead of ContinueSession because the
// guarantee under test is the dispatcher's, not ContinueSession's
// response-application path. Going through ContinueSession would couple the
// race to whether each caller's NextPipelineWorkItem read happens before or
// after the winner's insert, which is unrelated to what we want to assert.
func TestReviewer_ConcurrentContinuations_EnqueueReflectOnce(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	seedHypothesisTransition(t, svc, branch)
	sess := manualSession(t, svc, branch)

	const callers = 2
	var wg sync.WaitGroup
	wg.Add(callers)
	results := make([]*ReviewResult, callers)
	errs := make([]error, callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			fresh, err := svc.Pipeline().GetPipelineSession(ctx, sess.ID)
			if err != nil {
				errs[i] = err
				return
			}
			results[i], errs[i] = r.nextItem(ctx, fresh)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "caller %d errored", i)
		require.NotNilf(t, results[i], "caller %d returned nil result", i)
	}

	// The CAS guarantee: pipeline_work_items has at most one row for this
	// session regardless of who won. If the winner finished its insert
	// before the loser's reflect→done CAS, both callers see the reflect
	// item; if the loser raced past, the session is already done and the
	// winner's insert lands in a completed session — still one row, just
	// orphan. Two rows would mean the CAS broke.
	completed, remaining, err := svc.Pipeline().PipelineWorkItemStats(ctx, sess.ID)
	require.NoError(t, err)
	require.LessOrEqualf(t, completed+remaining, 1,
		"CAS broken: more than one reflect item enqueued (completed=%d remaining=%d)",
		completed, remaining)
}

// TestReviewer_ReflectAppliesReinforce is the end-to-end test for the new
// reflect contract: an agent's reinforce response submitted via
// ContinueSession actually appends the methodology path to the cited
// transition fact's refs, proving the dispatcher → parse → validate →
// apply path is wired and the resulting commit lands in git.
func TestReviewer_ReflectAppliesReinforce(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	// Seed both the methodology to reinforce and the hypothesis transition
	// the agent will cite. The session needs the transition to exist
	// between watermark and HEAD so the reflect step gets enqueued.
	const methPath = "kb/meta/reasoning/m.md"
	mf := fact.NewFact(methPath)
	mf.Title = "M"
	mf.Body = "methodology body"
	mf.Type = fact.Methodology
	mf.Confidence = 0.8
	mf.Sources = 1
	mfBody, err := fact.SerializeFact(mf)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, methPath, mfBody, "seed-methodology", "")
	require.NoError(t, err)

	seedHypothesisTransition(t, svc, branch)

	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	// Answer prune to advance into reflect phase.
	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.NotNil(t, res.Item)
	require.Equal(t, "reflect", res.Item.Type)

	// Submit reinforce response. transition path must match the path
	// seedHypothesisTransition uses ("kb/technology/h.md").
	reflectResp := `{
		"reasoning": "h ties to m",
		"reinforce": [{
			"methodology_path": "` + methPath + `",
			"transition_paths": ["kb/technology/h.md"],
			"rationale": "same reasoning shape"
		}],
		"propose": []
	}`
	res, err = r.ContinueSession(ctx, sess.ID, reflectResp)
	require.NoError(t, err)
	require.True(t, res.Done, "reinforce response should drive session to done")

	// The transition fact must now cite the methodology in its refs —
	// that's the canonical record of "this transition reinforced M". The ref is
	// stored in canonical kb://<own-id>/<path> form, like every other local ref
	// on every write path, so assert on the classified path rather than the raw
	// string the response happened to use.
	transResult, err := svc.Facts().ReadFact(ctx, branch, "kb/technology/h.md", nil)
	require.NoError(t, err)
	transFact, err := fact.ParseFact("kb/technology/h.md", transResult.Content)
	require.NoError(t, err)

	root, err := svc.RootCommit(ctx, branch)
	require.NoError(t, err)
	var citesMethodology bool
	for _, ref := range transFact.Refs {
		c := fact.ClassifyRef(ref, fact.ID12(root))
		if c.Kind == fact.RefLocalFact && c.Path == methPath {
			citesMethodology = true
		}
	}
	require.True(t, citesMethodology,
		"transition fact must ref the methodology that reinforced it, got %v", transFact.Refs)
}

// TestReviewer_ReflectRejectsBadResponse asserts that a malformed reflect
// response (transition path not in the session) errors and leaves the
// reflect work item unanswered, so the agent can retry. Pre-fix, the
// reflect case was a no-op: any garbage response was silently accepted.
func TestReviewer_ReflectRejectsBadResponse(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()
	branch := "agent/test"

	seedHypothesisTransition(t, svc, branch)
	sess := manualSession(t, svc, branch)
	insertManualPruneItem(t, svc, sess.ID, "kb/x.md")

	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions":[],"merges":[]}`)
	require.NoError(t, err)
	require.Equal(t, "reflect", res.Item.Type)

	// Bad response: transition path not from this session's transitions.
	bad := `{
		"reasoning": "wrong",
		"reinforce": [{
			"methodology_path": "kb/meta/reasoning/m.md",
			"transition_paths": ["kb/not-in-session.md"],
			"rationale": "x"
		}],
		"propose": []
	}`
	_, err = r.ContinueSession(ctx, sess.ID, bad)
	require.Error(t, err, "bad reflect response must be rejected")
	require.Contains(t, err.Error(), "kb/not-in-session.md")

	// The reflect work item must still be unanswered: phase stays at
	// reflect, session is not done. Agent can retry.
	got, err := svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "reflect", got.Phase, "phase must NOT advance on rejected reflect")
	require.Equal(t, "active", got.Status)
}

// newPhaseTestReviewer opens a fresh on-disk Service, initialises the agent
// branch, and returns a Reviewer wired against it. The caller drives the
// session manually via the returned svc.
func newPhaseTestReviewer(t *testing.T) (*Reviewer, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Seed the fact the shared distill fixtures cite. A distilled fact's refs
	// go through the same gate as every other write, so a response citing a
	// path that does not exist is rejected — correct behaviour, but not what
	// these dispatch/claim tests are about, so the fixture cites something real.
	seedFactWithSources(t, svc, "agent/test", "kb/technology/a.md", 2)

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return NewReviewer(ri, nil), svc
}

// manualSession creates a pipeline_sessions row directly via the index,
// bypassing StartSession (which clusters dirty facts and is heavy for tests
// that just want to exercise the dispatcher).
func manualSession(t *testing.T, svc *store.Service, branch string) *store.PipelineSession {
	t.Helper()
	sess, err := svc.Pipeline().CreatePipelineSession(context.Background(), "review", branch, "")
	require.NoError(t, err)
	return sess
}

// insertManualPruneItem queues a single prune work item with one synthetic
// fact path. The item's body is the empty-decisions/empty-merges shape, so
// any response of `{"decisions":[],"merges":[]}` validates and applies as a
// no-op — exercising the dispatcher without dragging in real LLM output.
func insertManualPruneItem(t *testing.T, svc *store.Service, sessionID, factPath string) {
	t.Helper()
	facts := []factForLLM{{File: factPath, Title: "x", Body: "x", Type: "observation"}}
	body, err := json.Marshal(facts)
	require.NoError(t, err)
	err = svc.Pipeline().InsertPipelineWorkItem(context.Background(), store.PipelineWorkItem{
		SessionID:  sessionID,
		StepType:   "prune",
		ClusterKey: "test-cluster",
		FactsJSON:  string(body),
		Priority:   0,
	})
	require.NoError(t, err)
}

// seedHypothesisTransition stages exactly one hypothesis-promoted transition
// between the watermark and HEAD: writes a hypothesis fact, sets the
// watermark to the resulting commit, then rewrites the same fact with a
// non-hypothesis type. findHypothesisTransitions will report this as a
// "promoted" transition and the reflect path will fire.
func seedHypothesisTransition(t *testing.T, svc *store.Service, branch string) {
	t.Helper()
	ctx := context.Background()
	const path = "kb/technology/h.md"

	hf := fact.NewFact(path)
	hf.Title = "H"
	hf.Body = "hypothesis under test"
	hf.Type = fact.Hypothesis
	hf.Confidence = 0.4
	hf.Sources = 1
	hyp, err := fact.SerializeFact(hf)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, path, hyp, "seed-hypothesis", "")
	require.NoError(t, err)

	head, err := svc.Branches().HeadCommit(ctx, branch)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))

	pf := fact.NewFact(path)
	pf.Title = "H"
	pf.Body = "promoted to principle"
	pf.Type = fact.Principle
	pf.Confidence = 0.8
	pf.Sources = 1
	prom, err := fact.SerializeFact(pf)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, path, prom, "promote-hypothesis", "")
	require.NoError(t, err)
}
