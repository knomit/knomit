package synthesize

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#118. When a backfill answer's motif fails validation, the write
// silently drops it: nothing in the answer result, nothing on the fact. The
// fact is then RE-OFFERED in a later session, because `motif_backfill_judged`
// records only recorded judgements and a wholly-refused batch records nothing.
//
// The loop self-heals in principle — a refused name gets another chance — but
// an agent repeating the same naming error loops on the same facts
// indefinitely, burning a backfill slot each cycle, with no error ever
// surfaced. Measured in the field as THREE offers: two silent refusals of the
// same 5-segment name in consecutive sessions, then resolution with a 4-segment
// name. The miscount survived its author's own review twice, which is the
// trap's point — `ceiling-set-by-give-up` reads as four concepts and is five
// hyphen segments.
//
// The plumbing already exists; the reasons simply never reached the answering
// agent. This carries them.
func TestApplyMotifBackfill_RefusedNameReachesTheAgent(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	d := env.deps()
	env.writeFact("kb/a.md", "A", "body a")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)

	// Five hyphen segments — the validator counts segments, not concepts, and
	// hyphenated compounds are the miscount trap.
	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/a.md", Motifs: []string{"ceiling-set-by-give-up"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, d, sess, env.branch, res,
		offeredBackfillForTest(t, ctx, env)))

	notices := strings.Join(sess.Health, "\n")
	require.NotEmpty(t, sess.Health,
		"a refused motif must reach the agent — re-offer is not a signal, it is "+
			"the absence of one")
	require.Contains(t, notices, "kb/a.md",
		"the notice names WHICH fact, or an agent holding eight cannot act on it")
	require.Contains(t, notices, "ceiling-set-by-give-up",
		"and WHICH name was refused")
	require.Contains(t, notices, "5",
		"and why, in the validator's own terms — 5 segments, not 'invalid'")
}

// The clean case must stay silent. A result that always carries refusal
// notices trains an agent to skip them, which is how the original silence
// comes back wearing text (the #130 drop-clause reasoning).
func TestApplyMotifBackfill_ValidAssignmentProducesNoNotice(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	d := env.deps()
	env.writeFact("kb/a.md", "A", "body a")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)

	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/a.md", Motifs: []string{"silent-fallback"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, d, sess, env.branch, res,
		offeredBackfillForTest(t, ctx, env)))

	require.Empty(t, sess.Health,
		"a clean assignment says nothing — notices are for refusals only")
}

// THE SUBJECT-STRIP HALF, which is a different failure and needs a different
// sentence. SerializeFact validates and THEN strips subject motifs without
// reporting it, so an answer made entirely of subject-restatements serializes
// cleanly to a fact with no motifs.
//
// That is not the refusal case above and must not read like it: a refused NAME
// can be fixed by naming it better next time, while a subject restatement has
// nothing to fix — the same content with the same hints draws the same answer
// forever. The agent needs to know which of the two happened, or it will spend
// its next turn rewording something that was never the problem.
func TestApplyMotifBackfill_SubjectStripIsReportedAsItsOwnCase(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	d := env.deps()
	// The subject set is entities + domain + PATH SEGMENTS, and every token of
	// the motif must be in it for the strip to fire. This path contributes
	// {kb, widget, cache}, so "widget-cache" is wholly a subject restatement —
	// a well-formed name that names what the fact is ABOUT rather than what it
	// is an instance of.
	env.writeFact("kb/widget/cache.md", "Cache", "body a")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)

	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/widget/cache.md", Motifs: []string{"widget-cache"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, d, sess, env.branch, res,
		offeredBackfillForTest(t, ctx, env)))

	notices := strings.ToLower(strings.Join(sess.Health, "\n"))
	require.NotEmpty(t, sess.Health,
		"the agent judged this fact and only restatements came back — it must be told")
	require.Contains(t, notices, "subject",
		"and told WHICH failure this was: a subject restatement, not a malformed "+
			"name — the two have different remedies and one of them is 'nothing to fix'")
}

// THE WIRING HALF, and the one that matters. The tests above assert on
// sess.Health — the SOURCE. What the answering agent receives is the
// ReviewResult of its own continue call, and before this fix that result
// carried no health at all: health rode StartSession's result only.
//
// So a notice appended during Apply and never attached to the turn's result
// would satisfy every assertion above while reaching nobody — the campaign's
// layer-confused disguise, in the exact place this fix is about. This drives
// the real continue path and reads what the caller gets.
func TestContinueSession_BackfillRefusalReachesTheCaller(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/a.md", "A", "body a")

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)

	offered := offeredBackfillForTest(t, ctx, env)
	blob, err := json.Marshal(offered)
	require.NoError(t, err)
	require.NoError(t, env.svc.Pipeline().InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   motifBackfillStepType,
		ClusterKey: "motif-backfill",
		FactsJSON:  string(blob),
		Priority:   motifBackfillPriority,
	}))

	r := NewReviewerWithOptions(env.ri, nil, EffortMedium, ScopeFilter{})

	served, err := r.PageItem(ctx, sess.ID, 0, 1)
	require.NoError(t, err)
	require.NotNil(t, served.Item, "precondition: the backfill item is current")

	// The measured trap: five hyphen segments reading as four concepts.
	res, err := r.ContinueSessionForItem(ctx, sess.ID,
		`{"assignments":[{"path":"kb/a.md","motifs":["ceiling-set-by-give-up"]}]}`,
		served.Item.ID)
	require.NoError(t, err)

	notices := strings.Join(res.Health, "\n")
	require.NotEmpty(t, res.Health,
		"the refusal must reach the CALLER's result, not merely the session "+
			"object — sess.Health is the source of a copy, and before this fix "+
			"nothing copied it onto a continue turn")
	require.Contains(t, notices, "kb/a.md")
	require.Contains(t, notices, "ceiling-set-by-give-up")
}

// sessionForBackfillTest makes a throwaway session for tests that drive
// applyMotifBackfill directly and do not read its notices.
func sessionForBackfillTest(t *testing.T, ctx context.Context, env *restatementEnv) *store.PipelineSession {
	t.Helper()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)
	return sess
}
