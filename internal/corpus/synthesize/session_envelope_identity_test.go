package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// knomit#113. knomit_review takes no repo/mount argument — correct, the binding
// decides — but the response envelope carried no repo identity either. A
// session operating through a multi-mount lens could not confirm which corpus
// it seeded from or where its answers would write; the fleet derived it from
// path-prefix formatting, closed via a fact whose own refs carried its kb://
// spelling. That is a correct instrument nobody should have had to invent.
//
// knomit_learn already returns where it wrote. This makes review match.
//
// Asserted on ReviewResult — the WIRE type — not on PipelineResult. The engine
// struct is the source of a copy; what a client receives is what matters, and
// the projection is a boundary this output has to cross.
func TestStartSession_EnvelopeCarriesRepoIdentity(t *testing.T) {
	ctx := context.Background()
	r, _ := newPhaseTestReviewer(t)

	res, err := r.StartSession(ctx)
	require.NoError(t, err)

	require.Equal(t, "test", res.Repo,
		"the envelope names the repo the session seeded from")
	require.NotEmpty(t, res.RepoID,
		"and its id, since a name is not unique across mounts")
	require.Equal(t, "agent/test", res.WriteBranch,
		"and the branch answers will write to — the question the fleet had to "+
			"answer by reading path prefixes")
}

// knomit#121's code-side residue, folded in here — AND CORRECTED.
//
// The residue was filed as "the work-stealing pickup is invisible to the
// resuming caller — no line says you resumed a session someone else opened".
// Reading CreatePipelineSession, that is not what happens: starting a session
// ABANDONS any active session for the same tool+branch and creates a fresh one.
// Nobody resumes anything.
//
// The invisibility is real and worse than filed, because it is two-sided:
//   - the NEW caller is not told it just abandoned an in-flight session;
//   - the ABANDONED caller is not told either. It finds out only when its next
//     continue call fails with "session is abandoned, not active" — after it
//     has spent a turn composing an answer to an item that no longer exists.
//
// So the envelope reports the abandonment, which is the observable and
// actionable fact. Nothing here can notify the loser; making the winner's
// result say what it displaced is what the campaign can honestly do.
func TestStartSession_EnvelopeReportsAnAbandonedSession(t *testing.T) {
	ctx := context.Background()
	r := newEffortTestReviewer(t, EffortNormal)

	first, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, first.SessionID)
	// PRECONDITION, asserted: the first session must still be ACTIVE, or there
	// is nothing to abandon and the assertion below passes on an empty field
	// comparing to an empty field. A one-fact corpus completes immediately and
	// makes this test vacuous — which is exactly what the first fixture did.
	require.False(t, first.Done,
		"precondition: the first session must be in flight, or nothing is displaced")
	require.Empty(t, first.AbandonedSession,
		"the first session displaced nothing, and must not claim to")

	second, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEqual(t, first.SessionID, second.SessionID,
		"precondition: starting again really does mint a new session")

	require.Equal(t, first.SessionID, second.AbandonedSession,
		"the new session must name the one it displaced — otherwise the "+
			"abandonment is invisible to everyone, and the loser discovers it "+
			"only when its next answer is rejected")
}

// The abandoned session really is unusable, which is what makes the report
// worth carrying. Without this the field could name a session that was fine,
// and the notice would be alarming rather than informative.
func TestStartSession_AbandonedSessionCannotBeContinued(t *testing.T) {
	ctx := context.Background()
	r := newEffortTestReviewer(t, EffortNormal)

	first, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.False(t, first.Done,
		"precondition: the first session has work in flight to lose")

	_, err = r.StartSession(ctx)
	require.NoError(t, err)

	_, err = r.ContinueSession(ctx, first.SessionID, `{"decisions":[],"merges":[]}`)
	require.Error(t, err,
		"the displaced session is dead — the reported abandonment is a real "+
			"loss of in-flight work, not a bookkeeping note")
	require.Contains(t, err.Error(), "abandoned")
}
