package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// AnsweredDistillResponses is the read path for the durable record of what a
// distill item decided. PendingPipelineWorkItems is its exact complement and
// returns only items whose response IS NULL, so before this there was no way
// to read an answer back out of a session at all.
func TestAnsweredDistillResponses(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	pi := svc.Pipeline()
	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)
	other, err := pi.CreatePipelineSession(ctx, "review", "agent/other", "")
	require.NoError(t, err)

	insert := func(sessionID, step, key string) int64 {
		require.NoError(t, pi.InsertPipelineWorkItem(ctx, PipelineWorkItem{
			SessionID: sessionID, StepType: step, ClusterKey: key, FactsJSON: "[]",
		}))
		items, err := pi.PendingPipelineWorkItems(ctx, sessionID)
		require.NoError(t, err)
		return items[len(items)-1].ID
	}

	answeredA := insert(sess.ID, "distill", "distill-rest-0")
	answeredB := insert(sess.ID, "distill", "distill-promoted-c1-0")
	unanswered := insert(sess.ID, "distill", "distill-rest-1")
	pruneItem := insert(sess.ID, "prune", "cluster-0")
	otherSession := insert(other.ID, "distill", "distill-rest-0")

	for id, resp := range map[int64]string{
		answeredA:    `{"synthesize": [], "declined_reason": "rest-bucket-incoherent"}`,
		answeredB:    `{"synthesize": [{"path": "kb/x.md"}]}`,
		pruneItem:    `{"decisions": []}`,
		otherSession: `{"synthesize": [], "declined_reason": "members-conflict"}`,
	} {
		ok, err := pi.AnswerPipelineWorkItem(ctx, id, resp)
		require.NoError(t, err)
		require.True(t, ok)
	}
	_ = unanswered

	got, err := pi.AnsweredDistillResponses(ctx, sess.ID)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{
		`{"synthesize": [], "declined_reason": "rest-bucket-incoherent"}`,
		`{"synthesize": [{"path": "kb/x.md"}]}`,
	}, got,
		"must return answered DISTILL responses for THIS session only — not the "+
			"unanswered item, not the prune item, not the other session's")
}

// A session with no answered distill items returns nothing rather than erroring,
// because completeSession calls this on every session including prune-only ones.
func TestAnsweredDistillResponses_EmptyWhenNoneAnswered(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)

	got, err := svc.Pipeline().AnsweredDistillResponses(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, got)
}
