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
	svc    *store.Service
	dbPath string
}

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
	return &resumeFixture{r: NewReviewer(ri, nil), svc: svc, dbPath: dbPath}
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

	beforeClaim = func(context.Context, int64) {
		claimed, cerr := f.svc.Pipeline().AnswerPipelineWorkItem(ctx, itemID, `"the other caller"`)
		require.NoError(t, cerr)
		require.True(t, claimed)
	}
	t.Cleanup(func() { beforeClaim = func(context.Context, int64) {} })

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
