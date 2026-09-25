package synthesize

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// resumeFixture is a reviewer over a corpus with enough facts that a start
// leaves an active session holding an item.
type resumeFixture struct {
	r      *Reviewer
	ri     *repos.RepoInstance
	svc    *store.Service
	dbPath string
}

// another is a second caller's reviewer over the same repo.
func (f *resumeFixture) another() *Reviewer { return NewReviewer(f.ri, nil) }

const resumeBranch = "agent/test"

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, resumeBranch))
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		f := fact.NewFact("kb/test/" + slug + ".md")
		f.Title = slug
		f.Body = "body of " + slug
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(context.Background(), resumeBranch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: resumeBranch, Svc: svc, OntologyRoot: "kb",
	})
	return &resumeFixture{r: NewReviewer(ri, nil), ri: ri, svc: svc, dbPath: dbPath}
}

// planningSession creates the row a start creates before it has planned: the
// slot is taken, the start key matches, and nothing is queued yet.
func (f *resumeFixture) planningSession(t *testing.T) *store.PipelineSession {
	t.Helper()
	sess, err := f.svc.Pipeline().CreatePipelineSessionReplacing(context.Background(),
		"review", resumeBranch, "mcp-session:planner", f.r.p.startKey(), "")
	require.NoError(t, err)
	return sess
}

func (f *resumeFixture) backdate(t *testing.T, id string, d time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite3", store.SessionDBPathFor(f.dbPath)+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`UPDATE pipeline_sessions SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-d).Format(time.RFC3339), id)
	require.NoError(t, err)
}

func (f *resumeFixture) status(t *testing.T, id string) string {
	t.Helper()
	sess, err := f.svc.Pipeline().GetPipelineSession(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	return sess.Status
}

func (f *resumeFixture) watermark(t *testing.T) string {
	t.Helper()
	wm, err := f.svc.Pipeline().GetPipelineWatermark(context.Background(), "review", resumeBranch)
	require.NoError(t, err)
	return wm
}

var liveWindow = StartOptions{ResumeWindow: 10 * time.Minute}

// A start that finds the slot held by a session still planning must not resume
// it: its queue is empty only because nothing has been queued yet, and a
// resumer would advance it to done, complete it, and move the watermark past
// seeds the planner has not served.
func TestStartOrResume_SessionStillPlanningIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	planning := f.planningSession(t)
	before := f.watermark(t)

	_, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")
	require.Contains(t, err.Error(), planning.ID)

	require.Equal(t, "active", f.status(t, planning.ID), "the planner keeps its session")
	require.Equal(t, before, f.watermark(t), "nothing completed, so the watermark did not move")
}

// Planning can outlast the resume window on a large corpus. A planning session
// is never stale by the window; only takeover or the reaper displaces it.
func TestStartOrResume_LongPlanningIsNotDisplacedByTheWindow(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	planning := f.planningSession(t)
	f.backdate(t, planning.ID, time.Hour)

	_, err := f.r.StartOrResumeSession(ctx, StartOptions{ResumeWindow: time.Minute})
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")
	require.Equal(t, "active", f.status(t, planning.ID))
}

func TestStartOrResume_TakeoverDisplacesAPlanningSession(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	planning := f.planningSession(t)

	res, err := f.r.StartOrResumeSession(ctx, StartOptions{ResumeWindow: 10 * time.Minute, Takeover: true})
	require.NoError(t, err)
	require.Equal(t, planning.ID, res.AbandonedSession)
	require.Equal(t, "abandoned", f.status(t, planning.ID))
}

// Nobody but the planner holds a planning session's id, but if one arrives it
// is refused rather than served the empty queue.
func TestContinue_SessionStillPlanningIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	planning := f.planningSession(t)

	_, err := f.r.ContinueSessionForItem(ctx, planning.ID, `{"decisions":[],"merges":[]}`, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")
	require.Equal(t, "active", f.status(t, planning.ID))
}

// Once planning has finished, a second start resumes the session, and planning
// finishing is itself a heartbeat.
func TestStartOrResume_AfterPlanningResumes(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)

	first, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)
	require.False(t, first.Done, "precondition: the session holds an item")

	second, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)
	require.True(t, second.Resumed)
	require.Equal(t, first.SessionID, second.SessionID)
}

// answerFor is a well-formed answer to the item a start served.
func answerFor(t *testing.T, res *ReviewResult) string {
	t.Helper()
	require.NotNil(t, res.Item)
	switch res.Item.Type {
	case "distill":
		return `{"synthesize":[],"retract":[],"declined_reason":"no-shared-mechanism"}`
	case "prune":
		return `{"decisions":[],"merges":[]}`
	}
	t.Fatalf("no canned answer for item type %q", res.Item.Type)
	return ""
}

// Two callers answer the same item and both pass the item check; the one
// that loses the claim is told so, and is not handed the next item as though
// its answer had landed.
func TestContinue_LostClaimIsAnError(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	res, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)
	require.NotNil(t, res.Item)
	itemID := res.Item.ID

	f.r.p.hooks.beforeClaim = func(context.Context, int64) {
		claimed, cerr := f.svc.Pipeline().AnswerPipelineWorkItem(ctx, itemID, `"the other caller"`)
		require.NoError(t, cerr)
		require.True(t, claimed)
	}

	_, err = f.r.ContinueSessionForItem(ctx, res.SessionID, answerFor(t, res), itemID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered by another caller")
	require.Contains(t, err.Error(), "nothing was applied")
}

// An answer naming an item when nothing is outstanding is stale; it is
// refused, not silently turned into the session's next step.
func TestContinue_StaleItemWithEmptyQueueIsAnError(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	sess := manualSession(t, f.svc, resumeBranch)

	_, err := f.r.ContinueSessionForItem(ctx, sess.ID, `{"decisions":[],"merges":[]}`, 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "item 5 is no longer outstanding")
	require.Contains(t, err.Error(), "no response")
	require.Equal(t, "active", f.status(t, sess.ID), "a refused answer must not advance the session")
}

// The item-mismatch refusal says how to get the current item.
func TestContinue_StaleItemSaysHowToGetTheCurrentOne(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	res, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)

	_, err = f.r.ContinueSessionForItem(ctx, res.SessionID, answerFor(t, res), res.Item.ID+100)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is current")
	require.Contains(t, err.Error(), "no response")
}

// Current serves the outstanding item again without answering or advancing
// anything.
func TestCurrent_ServesTheOutstandingItemWithoutAdvancing(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	res, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)

	again, err := f.r.Current(ctx, res.SessionID)
	require.NoError(t, err)
	require.Equal(t, res.SessionID, again.SessionID)
	require.Equal(t, res.Item.ID, again.Item.ID)
	require.Equal(t, res.Progress.Completed, again.Progress.Completed)
	require.NotEmpty(t, again.Item.Prompt, "the whole item, from page 1")
}

// The start key compares scopes the way the scope filter reads them: case and
// repetition do not make two scopes different.
func TestStartKey_IgnoresCaseOrderAndRepeats(t *testing.T) {
	key := func(domain, entities []string) string {
		return (&Pipeline{effort: EffortNormal, scope: ScopeFilter{Domain: domain, Entities: entities}}).startKey()
	}
	require.Equal(t,
		key([]string{"mcp", "store"}, []string{"knomit_review"}),
		key([]string{"Store", "MCP", "mcp"}, []string{"KNOMIT_REVIEW", "knomit_review"}))
	require.NotEqual(t, key([]string{"mcp"}, nil), key([]string{"store"}, nil))
	require.NotEqual(t, key(nil, nil),
		(&Pipeline{effort: EffortHigh}).startKey(), "effort is part of the key")
}

// An item that has been claimed but not yet applied is still outstanding. A
// second caller that finds no unanswered item while the first is applying the
// last one must not advance the session: the apply may enqueue follow-up
// items, and completing the session would strand them and move the watermark.
func TestApplying_ItemBeingAppliedHoldsThePhase(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	a, b := f.r, f.another()

	res, err := a.StartOrResumeSession(ctx, liveWindow)
	require.NoError(t, err)
	require.NotNil(t, res.Item)
	pending, err := f.svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "precondition: A is answering the last item")

	var followUp int64
	a.p.hooks.duringApply = func(ctx context.Context, _ int64) {
		_, cerr := b.Current(ctx, res.SessionID)
		require.Error(t, cerr, "B must not step the session while A applies")
		require.Contains(t, cerr.Error(), "being applied by another caller")

		sess, gerr := f.svc.Pipeline().GetPipelineSession(ctx, res.SessionID)
		require.NoError(t, gerr)
		require.Equal(t, "active", sess.Status)
		require.Equal(t, "work", sess.Phase)

		// The apply enqueues a follow-up, as a distill that synthesizes does.
		require.NoError(t, f.svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID: res.SessionID, StepType: pending[0].StepType, ClusterKey: "follow-up",
			FactsJSON: pending[0].FactsJSON, Priority: -1, Depth: 1,
		}))
		next, nerr := f.svc.Pipeline().NextPipelineWorkItem(ctx, res.SessionID)
		require.NoError(t, nerr)
		followUp = next.ID
	}

	_, err = a.ContinueSessionForItem(ctx, res.SessionID, answerFor(t, res), res.Item.ID)
	require.NoError(t, err)

	cur, err := b.Current(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotNil(t, cur.Item, "after A's apply, B is served A's follow-up")
	require.Equal(t, followUp, cur.Item.ID)
}

// Every create is planning until planned, including the in-process start
// (RunAll, the web synthesis job). A caller handed such a session's id cannot
// run its empty queue to completion, and a start does not resume it.
func TestPlanning_InProcessCreateIsPlanningUntilPlanned(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	sess, err := f.svc.Pipeline().CreatePipelineSession(ctx, "review", resumeBranch, "")
	require.NoError(t, err)
	before := f.watermark(t)

	_, err = f.r.Current(ctx, sess.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")

	_, err = f.r.StartOrResumeSession(ctx, liveWindow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")

	require.Equal(t, "active", f.status(t, sess.ID))
	require.Equal(t, before, f.watermark(t))
}

// A planner displaced by takeover must not complete the row it no longer owns
// or move the watermark while the taker plans over the same seeds.
func TestComplete_DisplacedPlannerCompletesNothing(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	displaced := f.planningSession(t)
	before := f.watermark(t)

	taken, err := f.another().StartOrResumeSession(ctx, StartOptions{ResumeWindow: 10 * time.Minute, Takeover: true})
	require.NoError(t, err)
	require.Equal(t, displaced.ID, taken.AbandonedSession)

	_, _ = f.r.p.completeSession(ctx, displaced)
	require.Equal(t, "abandoned", f.status(t, displaced.ID), "the displaced row stays abandoned")
	require.Equal(t, before, f.watermark(t), "and the watermark does not move")
	require.Equal(t, "active", f.status(t, taken.SessionID))
}

// An in-process run (RunAll, the web synthesis job) has no session an agent
// can continue, so a start is not pointed at its session_id.
func TestStartOrResume_InProcessSessionIsNamedAsSuch(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	sess, err := f.svc.Pipeline().CreatePipelineSession(ctx, "review", resumeBranch, "")
	require.NoError(t, err)
	require.NoError(t, f.svc.Pipeline().MarkPipelineSessionPlanned(ctx, sess.ID))

	_, err = f.r.StartOrResumeSession(ctx, liveWindow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "an in-process review is running on this branch")
	require.Contains(t, err.Error(), "takeover:true")
	require.NotContains(t, err.Error(), sess.ID, "there is nothing for the caller to continue")
}

// A plan that hangs holds the slot until the reaper; the refusal says how to
// take it instead of only "retry".
func TestStartOrResume_StillPlanningOffersTakeover(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t)
	f.planningSession(t)

	_, err := f.r.StartOrResumeSession(ctx, liveWindow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "still planning")
	require.Contains(t, err.Error(), "takeover:true")
}

// The key encodes each list as a list, so a comma inside one value never
// equals two values, and it folds tags exactly as the scope filter does.
func TestStartKey_EncodesListsAndFoldsLikeTheFilter(t *testing.T) {
	key := func(domain, entities []string) string {
		return (&Pipeline{effort: EffortNormal, scope: ScopeFilter{Domain: domain, Entities: entities}}).startKey()
	}
	require.NotEqual(t, key([]string{"a,b"}, nil), key([]string{"a", "b"}, nil))
	require.NotEqual(t, key(nil, []string{"a,b"}), key(nil, []string{"a", "b"}))
	require.Equal(t, key([]string{"store sqlite"}, nil), key([]string{"store-sqlite"}, nil),
		"a hyphen and a space are one tag to the domain filter")
	// Unicode forms follow the filter too: DomainTagMatches normalises to NFC,
	// EntityTagMatches only case-folds, so NFC and NFD are one domain but two
	// entities.
	require.True(t, store.DomainTagMatches("caf\u00e9", "cafe\u0301"))
	require.Equal(t, key([]string{"caf\u00e9"}, nil), key([]string{"cafe\u0301"}, nil))
	require.False(t, store.EntityTagMatches("caf\u00e9", "cafe\u0301"))
	require.NotEqual(t, key(nil, []string{"caf\u00e9"}), key(nil, []string{"cafe\u0301"}))
}
