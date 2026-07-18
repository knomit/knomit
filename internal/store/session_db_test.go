package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openSessionTestStore opens a fresh store (which creates the ephemeral session
// DB) and returns it. No InitRepo: session state has no FK to branches/facts.
func openSessionTestStore(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestSessionTables_RelocatedOutOfMainDB pins the isolation invariant: after the
// 000010 migration the in-flight session/work-queue tables live ONLY in the
// ephemeral session DB, while pipeline_watermarks (durable progress) stays in
// the main DB.
func TestSessionTables_RelocatedOutOfMainDB(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()

	relocated := []string{"tool_sessions", "tool_queue", "tool_seen_paths", "pipeline_sessions", "pipeline_work_items"}

	mainHas := func(table string) bool {
		var n int
		err := svc.rh.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		require.NoError(t, err)
		return n > 0
	}
	sessionHas := func(table string) bool {
		var n int
		err := svc.sessionDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		require.NoError(t, err)
		return n > 0
	}

	for _, tbl := range relocated {
		require.Falsef(t, mainHas(tbl), "%s must NOT exist in the main DB after relocation", tbl)
		require.Truef(t, sessionHas(tbl), "%s must exist in the ephemeral session DB", tbl)
	}

	// Durable progress stays in the main DB and is NOT relocated.
	require.True(t, mainHas("pipeline_watermarks"), "pipeline_watermarks must remain in the main DB")
	require.False(t, sessionHas("pipeline_watermarks"), "pipeline_watermarks must NOT be in the session DB")
}

// TestToolSession_BindingRoundTrip pins that a tool session records the binding
// name it was minted under and returns it on read — the anchor for the
// foreign-binding resume rejection (lenses RFC §7.3).
func TestToolSession_BindingRoundTrip(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()
	ti := svc.ti

	created, err := ti.CreateToolSession(ctx, "query", "agent/x", "", "my-lens", "")
	require.NoError(t, err)
	require.Equal(t, "my-lens", created.Binding, "created session must carry the binding")

	got, err := ti.GetToolSession(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "my-lens", got.Binding, "binding must round-trip through the session DB")
}

// TestToolSession_ReadSetRoundTrip pins that a tool session records the read-set
// fingerprint it was minted with and returns it on read — the anchor for the
// re-pinned-read-set resume rejection (M-2 / lenses RFC §7.3).
func TestToolSession_ReadSetRoundTrip(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()
	ti := svc.ti

	const fp = "aaaaaaaaaaaa@agent/x,bbbbbbbbbbbb@main"
	created, err := ti.CreateToolSession(ctx, "query", "agent/x", "", "my-lens", fp)
	require.NoError(t, err)
	require.Equal(t, fp, created.ReadSet, "created session must carry the read-set fingerprint")

	got, err := ti.GetToolSession(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, fp, got.ReadSet, "read-set fingerprint must round-trip through the session DB")
}

// TestReapIdleSessions_RemovesIdleKeepsActive verifies the time-based reaper:
// a session idle longer than its TTL (and its queued children, via cascade) is
// deleted, while an actively-used session is kept.
func TestReapIdleSessions_RemovesIdleKeepsActive(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()
	ti := svc.ti

	idle, err := ti.CreateToolSession(ctx, "query", "agent/test", "", "test", "")
	require.NoError(t, err)
	active, err := ti.CreateToolSession(ctx, "query", "agent/test", "", "test", "")
	require.NoError(t, err)

	// Give the idle session some queued rows so we can confirm cascade cleanup.
	require.NoError(t, ti.EnqueuePaths(ctx, idle.ID, []QueueItem{
		{Path: "kb/a.md", CommitHash: "c1", SortKey: 0, State: "{}"},
		{Path: "kb/b.md", CommitHash: "c2", SortKey: 1, State: "{}"},
	}))

	// Backdate the idle session well past the TTL; leave active at "now".
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	_, err = svc.sessionDB.ExecContext(ctx,
		`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, old, idle.ID)
	require.NoError(t, err)

	n, err := svc.ReapIdleSessions(ctx, 1*time.Minute, 1*time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, n, "exactly the idle session should be reaped")

	gone, err := ti.GetToolSession(ctx, idle.ID)
	require.NoError(t, err)
	require.Nil(t, gone, "idle session must be deleted")

	// Cascade: its queued rows are gone too.
	size, err := ti.QueueSize(ctx, idle.ID)
	require.NoError(t, err)
	require.Zero(t, size, "idle session's queue rows must be cascade-deleted")

	stillThere, err := ti.GetToolSession(ctx, active.ID)
	require.NoError(t, err)
	require.NotNil(t, stillThere, "active session must be kept")
}

// TestReapIdleSessions_ZeroTTLSkips confirms a non-positive TTL disables reaping
// for that cluster.
func TestReapIdleSessions_ZeroTTLSkips(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()

	sess, err := svc.ti.CreateToolSession(ctx, "query", "agent/test", "", "test", "")
	require.NoError(t, err)
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	_, err = svc.sessionDB.ExecContext(ctx,
		`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, old, sess.ID)
	require.NoError(t, err)

	n, err := svc.ReapIdleSessions(ctx, 0, 0)
	require.NoError(t, err)
	require.Zero(t, n, "TTL=0 must skip reaping")
	got, err := svc.ti.GetToolSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got, "session must survive when TTL=0")
}

// TestMarkPipelineSessionScoped_SetsFlag is the regression guard for the
// watermark-poisoning fix: a scoped session must be markable so that
// hypothesizeNextItem can skip watermark advancement at completion. Before the
// fix, there was no way to record scope on a session — every session advanced
// the watermark unconditionally.
func TestMarkPipelineSessionScoped_SetsFlag(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)
	require.False(t, sess.Scoped, "newly created session must not be scoped")

	// GetPipelineSession also returns Scoped=false before marking.
	got, err := svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.False(t, got.Scoped, "GetPipelineSession must return Scoped=false before MarkPipelineSessionScoped")

	require.NoError(t, svc.Pipeline().MarkPipelineSessionScoped(ctx, sess.ID))

	got, err = svc.Pipeline().GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.Scoped, "GetPipelineSession must return Scoped=true after MarkPipelineSessionScoped")
}

// TestDequeuePaths_PreservesSortOrderAndState pins that the queue returns items
// in sort_key order and round-trips the per-item state payload, and that a
// dequeue bumps the session's last_used_at heartbeat.
func TestDequeuePaths_PreservesSortOrderAndState(t *testing.T) {
	svc := openSessionTestStore(t)
	ctx := context.Background()
	ti := svc.ti

	sess, err := ti.CreateToolSession(ctx, "query", "agent/test", "", "test", "")
	require.NoError(t, err)

	// Backdate so we can detect the heartbeat bump on dequeue.
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	_, err = svc.sessionDB.ExecContext(ctx,
		`UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, old, sess.ID)
	require.NoError(t, err)

	// Enqueue out of sort order; expect dequeue to return sorted by sort_key.
	require.NoError(t, ti.EnqueuePaths(ctx, sess.ID, []QueueItem{
		{Path: "kb/c.md", CommitHash: "c", SortKey: 2, State: `{"n":2}`},
		{Path: "kb/a.md", CommitHash: "a", SortKey: 0, State: `{"n":0}`},
		{Path: "kb/b.md", CommitHash: "b", SortKey: 1, State: `{"n":1}`},
	}))

	items, err := ti.DequeuePaths(ctx, sess.ID, 10)
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"}, []string{items[0].Path, items[1].Path, items[2].Path})
	require.Equal(t, `{"n":0}`, items[0].State)
	require.Equal(t, `{"n":2}`, items[2].State)

	// Heartbeat: last_used_at must have advanced past the backdated value.
	var lastUsed string
	require.NoError(t, svc.sessionDB.QueryRowContext(ctx,
		`SELECT last_used_at FROM tool_sessions WHERE id = ?`, sess.ID).Scan(&lastUsed))
	require.Greater(t, lastUsed, old, "dequeue must bump last_used_at")
}
