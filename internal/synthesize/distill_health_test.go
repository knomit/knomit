package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// A session whose distill item produced a synthesis is not a decline, and a
// session with no declines gets no line at all rather than a zero line.
func TestDistillDeclines_NoLineWhenNothingDeclined(t *testing.T) {
	r, svc := newPhaseTestReviewer(t)
	ctx := context.Background()

	seedObservation(t, svc, "agent/test", "kb/technology/a.md")
	sess := manualSession(t, svc, "agent/test")
	insertManualDistillItem(t, svc, sess.ID)

	res, err := r.ContinueSession(ctx, sess.ID, distillResponseOneFact)
	require.NoError(t, err)
	require.True(t, res.Done)
	require.NotContains(t, strings.Join(res.Health, "\n"), "distill declines",
		"a session that synthesized must not report a decline line")
}
