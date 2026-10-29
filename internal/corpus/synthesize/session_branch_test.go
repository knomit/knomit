package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/step"
)

// A session_id continues a session only on the branch the session was opened
// on: a caller bound to another branch of the same repo reads it exactly as a
// session that does not exist (the session_id is a secret, not a reach). The
// caller on the session's own branch is served.
//
// Sabotage: drop the `sess.Branch != p.branch` clause in activeSession and the
// experiment-bound caller is served the agent branch's session.
func TestActiveSession_RefusesASessionOnAnotherBranch(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	agent := ri.AgentBranch()
	exp, err := svc.Merge().OpenExperiment(ctx, "elsewhere", "", agent)
	require.NoError(t, err)

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, step.ReviewTool, agent, "")
	require.NoError(t, err)
	marked, err := svc.Pipeline().MarkPipelineSessionPlanned(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, marked)

	onExp := NewReviewerOnBranch(ri, nil, step.EffortNormal, step.ScopeFilter{}, exp.Branch())
	_, err = onExp.Current(ctx, sess.ID)
	require.Error(t, err)
	_, ghost := onExp.Current(ctx, "no-such-session")
	require.Error(t, ghost)
	require.Equal(t, ghost.Error(), strings.ReplaceAll(err.Error(), sess.ID, "no-such-session"),
		"a session on another branch must read exactly like one that does not exist")

	_, err = NewReviewerOnBranch(ri, nil, step.EffortNormal, step.ScopeFilter{}, agent).Current(ctx, sess.ID)
	if err != nil {
		require.NotContains(t, err.Error(), "not found", "the session's own branch is served")
	}
}
