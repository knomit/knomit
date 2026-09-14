package synthesize

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Zero declines is a RENDERING path, not an early return, so it needs its own
// case: joining an empty breakdown would otherwise print "0 of 9 items ()".
func TestDistillDeclineHealthLine_NoneDeclined(t *testing.T) {
	require.Equal(t, "distill declines: none of 9 items",
		distillDeclineHealthLine(map[string]int{}, 9))
}

func TestDistillDeclineHealthLine(t *testing.T) {
	line := distillDeclineHealthLine(map[string]int{"rest-bucket-incoherent": 3, "no-shared-mechanism": 2, "": 1}, 9)
	require.Equal(t,
		"distill declines: 6 of 9 items (no-shared-mechanism 2, rest-bucket-incoherent 3, unstated 1)",
		line)
}

// The decline line must reach the COMPLETION result, which is the only turn on
// which it can exist: a decline is not knowable until its item is answered, and
// the first result — which carries every other health descriptor — is composed
// before any item has been. This drives two real distill items through
// ContinueSession and asserts the line the agent actually receives.
func TestDistillDeclines_SurfaceOnTheCompletionResult(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	sess := manualSession(t, svc, "agent/test")
	insertManualDistillItem(t, svc, sess.ID)
	insertManualDistillItem(t, svc, sess.ID)

	res, err := r.ContinueSession(ctx, sess.ID,
		`{"synthesize": [], "retract": [], "declined_reason": "no-shared-mechanism", "declined_note": "two mechanisms"}`)
	require.NoError(t, err)
	require.False(t, res.Done, "the second distill item must still be pending")
	require.NotContains(t, strings.Join(res.Health, "\n"), "distill declines",
		"a mid-session turn must not carry the line: the tally is not complete yet")

	res, err = r.ContinueSession(ctx, sess.ID,
		`{"synthesize": [], "retract": []}`)
	require.NoError(t, err)
	require.True(t, res.Done)

	require.Contains(t, strings.Join(res.Health, "\n"),
		"distill declines: 2 of 2 items (no-shared-mechanism 1, unstated 1)",
		"both declines must be counted, and the one that gave no reason must be "+
			"named 'unstated' rather than hidden — that item is exactly the case "+
			"declined_reason exists to surface")
}

// A session with distill items that declined NOTHING still says so.
//
// This test previously pinned the opposite — no line when nothing declined —
// and that is the rule being reversed, so it is rewritten rather than deleted.
// The case it guards is still worth pinning; only the expected output changed.
//
// WHY THE REVERSAL: an absent line used to mean any of three different things —
// the session ran no distill items, it ran some and declined none, or the read
// of the durable record failed. One signal, three causes, and the failure was
// the wire-invisible one. A session that declined nothing now states it.
func TestDistillDeclines_ZeroIsStatedNotInferred(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	seedObservation(t, svc, "agent/test", "kb/technology/a.md")
	sess := manualSession(t, svc, "agent/test")
	insertManualDistillItem(t, svc, sess.ID)

	res, err := r.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.Contains(t, strings.Join(res.Health, "\n"), "distill declines: none of 1 items",
		"a session that synthesized and declined nothing must SAY zero, not go silent")
}

// The one remaining cause of an absent line: the session ran no distill items
// at all. That keeps the line meaningful — it is reported per distill session,
// not stamped on every session regardless of whether distill ran.
func TestDistillDeclines_NoLineWhenNoDistillItems(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	const prunePath = "kb/technology/a.md"
	seedObservation(t, svc, "agent/test", prunePath)
	sess := manualSession(t, svc, "agent/test")
	insertManualPruneItem(t, svc, sess.ID, prunePath)

	res, err := r.ContinueSession(ctx, sess.ID, `{"decisions": [], "merges": []}`)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotContains(t, strings.Join(res.Health, "\n"), "distill declines",
		"a prune-only session must not report a distill line; the line is about distill work that happened")
}

// failingPipelineIndex faults only AnsweredDistillResponses. The rest of the
// interface is embedded and never called; a nil-method panic would be a louder
// failure than a wrong assertion, which is the behaviour we want from a stub.
type failingPipelineIndex struct {
	store.PipelineIndex
}

func (failingPipelineIndex) AnsweredDistillResponses(context.Context, string) ([]string, error) {
	return nil, errors.New("session db unavailable")
}

// A failed read must SAY so. It used to return "" and look exactly like a clean
// session — one signal for two opposite situations, and the silent one was the
// failure. It stays fail-soft: the function returns no error at all, so a
// completion cannot fail over a descriptor whose mutations are already
// committed.
//
// This exercises distillDeclineHealth directly rather than through
// ContinueSession, because the engine reaches its indices through a concrete
// *store.Service and there is no seam to fault the real store mid-session —
// the same limitation pipeline.go records for the answer-write path.
func TestDistillDeclineHealth_StoreErrorSaysUnavailable(t *testing.T) {
	line := distillDeclineHealth(context.Background(),
		Deps{Pipeline: failingPipelineIndex{}}, "any-session")

	require.Equal(t, "distill declines: unavailable (store read failed)", line,
		"a failed read must be distinguishable from a session that declined nothing")
}
