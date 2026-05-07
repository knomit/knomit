package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMethodologyReinforcements_RoundTrip asserts the basic insert + query
// contract: rows persisted via InsertReinforcement come back through both
// the by-methodology and by-session lookups, in insertion order, with all
// fields preserved.
func TestMethodologyReinforcements_RoundTrip(t *testing.T) {
	svc := newReinforcementsTestService(t)
	mi := svc.Methodology()
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)

	// Two reinforcements against the same methodology, from two transitions
	// in the same session — the common shape when reflect explains multiple
	// transitions with one shared lesson.
	require.NoError(t, mi.InsertReinforcement(ctx, MethodologyReinforcement{
		Branch:          "agent/test",
		MethodologyPath: "kb/meta/reasoning/m1.md",
		TransitionPath:  "kb/h1.md",
		SessionID:       sess.ID,
		Rationale:       "h1 confirmed lesson m1",
	}))
	require.NoError(t, mi.InsertReinforcement(ctx, MethodologyReinforcement{
		Branch:          "agent/test",
		MethodologyPath: "kb/meta/reasoning/m1.md",
		TransitionPath:  "kb/h2.md",
		SessionID:       sess.ID,
		Rationale:       "h2 same pattern",
	}))
	// One reinforcement against a different methodology in the same session.
	require.NoError(t, mi.InsertReinforcement(ctx, MethodologyReinforcement{
		Branch:          "agent/test",
		MethodologyPath: "kb/meta/reasoning/m2.md",
		TransitionPath:  "kb/h3.md",
		SessionID:       sess.ID,
		Rationale:       "different lesson",
	}))

	byPath, err := mi.ListReinforcementsByPath(ctx, "agent/test", "kb/meta/reasoning/m1.md")
	require.NoError(t, err)
	require.Len(t, byPath, 2)
	require.Equal(t, "kb/h1.md", byPath[0].TransitionPath)
	require.Equal(t, "kb/h2.md", byPath[1].TransitionPath)
	require.Equal(t, "h1 confirmed lesson m1", byPath[0].Rationale)
	require.NotEmpty(t, byPath[0].ReinforcedAt, "reinforced_at must be stamped server-side")

	bySession, err := mi.ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, bySession, 3)

	// Sanity: no rows for an unrelated methodology path.
	none, err := mi.ListReinforcementsByPath(ctx, "agent/test", "kb/meta/reasoning/m-nope.md")
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestMethodologyReinforcements_BranchScoped ensures the by-path query
// honours the branch column — a methodology with the same path on a
// different branch is not returned.
func TestMethodologyReinforcements_BranchScoped(t *testing.T) {
	svc := newReinforcementsTestService(t)
	mi := svc.Methodology()
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)

	require.NoError(t, mi.InsertReinforcement(ctx, MethodologyReinforcement{
		Branch:          "agent/test",
		MethodologyPath: "kb/meta/reasoning/m.md",
		TransitionPath:  "kb/h.md",
		SessionID:       sess.ID,
		Rationale:       "x",
	}))

	got, err := mi.ListReinforcementsByPath(ctx, "main", "kb/meta/reasoning/m.md")
	require.NoError(t, err)
	require.Empty(t, got, "rows must be branch-scoped")
}

// TestMethodologyReinforcements_SessionCascadeDelete ensures the FK is set
// up correctly: deleting a pipeline session cleans up its reinforcements.
// Protects against orphan rows when sessions are GC'd.
func TestMethodologyReinforcements_SessionCascadeDelete(t *testing.T) {
	svc := newReinforcementsTestService(t)
	mi := svc.Methodology()
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)
	require.NoError(t, mi.InsertReinforcement(ctx, MethodologyReinforcement{
		Branch:          "agent/test",
		MethodologyPath: "kb/meta/reasoning/m.md",
		TransitionPath:  "kb/h.md",
		SessionID:       sess.ID,
		Rationale:       "x",
	}))

	_, err = svc.rh.db.ExecContext(ctx, `DELETE FROM pipeline_sessions WHERE id = ?`, sess.ID)
	require.NoError(t, err)

	got, err := mi.ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, got, "FK CASCADE must drop reinforcements when the session is deleted")
}

func newReinforcementsTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc
}
