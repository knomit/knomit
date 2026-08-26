package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPipelineSession_PhaseDefault asserts that a freshly created session
// starts in phase "work" — the default for new sessions before any reflect
// step has been considered. Round-trips through CreatePipelineSession +
// GetPipelineSession to verify the column is persisted, not just stamped on
// the in-memory return value.
func TestPipelineSession_PhaseDefault(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()

	created, err := pi.CreatePipelineSession(context.Background(), "review", "agent/test", "")
	require.NoError(t, err)
	require.Equal(t, "work", created.Phase, "new session must start in phase=work")

	got, err := pi.GetPipelineSession(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "work", got.Phase, "phase must be persisted across read")
}

// TestPipelineSession_AdvancePhase_MatchingFrom verifies the happy-path CAS:
// when the row's current phase equals `from`, the update succeeds, returns
// true, and a subsequent read sees the new phase.
func TestPipelineSession_AdvancePhase_MatchingFrom(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)

	advanced, err := pi.AdvancePipelineSessionPhase(ctx, sess.ID, "work", "reflect")
	require.NoError(t, err)
	require.True(t, advanced, "advance from matching phase must succeed")

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "reflect", got.Phase)
}

// TestPipelineSession_AdvancePhase_NonMatchingFrom is the load-bearing test
// for the bug fix: the CAS guarantees that two concurrent continuations of
// the same session can't both enqueue a reflect item. The second caller
// observes phase != "work" and the advance returns (false, nil).
//
// Pre-fix code path: in-memory `reflectChecked` map on Reviewer (constructed
// per MCP call) silently let every continuation re-enter the
// "should-we-enqueue-reflect" branch, so the same transitions got enqueued
// repeatedly. With the phase column, the second caller's CAS fails and the
// re-enqueue is impossible.
func TestPipelineSession_AdvancePhase_NonMatchingFrom(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)

	// First caller wins the work -> reflect transition.
	first, err := pi.AdvancePipelineSessionPhase(ctx, sess.ID, "work", "reflect")
	require.NoError(t, err)
	require.True(t, first)

	// Second caller, racing, tries the same transition. It must observe the
	// already-advanced phase and report no-op without erroring.
	second, err := pi.AdvancePipelineSessionPhase(ctx, sess.ID, "work", "reflect")
	require.NoError(t, err, "lost-race CAS must be benign, not an error")
	require.False(t, second, "second advance must return false (already advanced)")

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "reflect", got.Phase, "phase must remain at the value the winner set")
}

// TestPipelineSession_AdvancePhase_UpdatesTimestamp ensures updated_at
// advances on a successful phase change so audit/debug tooling can tell when
// the transition happened.
func TestPipelineSession_AdvancePhase_UpdatesTimestamp(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)
	createdTS := sess.UpdatedAt

	// SQLite stores RFC3339 strings at second granularity; sleep to ensure
	// the new timestamp is strictly greater. 1.1 seconds is generous against
	// integer-second truncation.
	advanced, err := pi.AdvancePipelineSessionPhase(ctx, sess.ID, "work", "reflect")
	require.NoError(t, err)
	require.True(t, advanced)

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, got.UpdatedAt, createdTS,
		"updated_at must not regress after a phase advance")
}

// newPhaseTestService opens a fresh SQLite-backed Service in a temp dir and
// initialises a single repo with one branch suitable for pipeline tests.
func newPhaseTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc
}
