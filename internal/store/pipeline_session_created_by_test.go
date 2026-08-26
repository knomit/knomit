package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// pipeline_sessions.created_by (knomit#123) records who opened a session, so
// that an unexpected session is attributable within its retention window.
//
// The fixture below is built to DISCRIMINATE, which for this column means one
// specific thing: two sessions created with two DIFFERENT handles must come
// back carrying their OWN handle. Asserting only "the value round-trips" would
// pass against an implementation that stores the first caller's handle on every
// row, or that reads the column from the wrong row — both of which produce a
// non-empty, plausible-looking answer to the forensic question and a wrong one.
//
// The two sessions are on DIFFERENT branches on purpose: CreatePipelineSession
// abandons any active session for the same tool+branch, so two on one branch
// would leave the first abandoned and make the test about lifecycle instead.
func TestCreatePipelineSession_CreatedByIsPerRow(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	const handleA = "mcp-session:aaaa-1111 client:claude-code/1.2.3"
	const handleB = "mcp-session:bbbb-2222 client:some-other-client/9"
	require.NotEqual(t, handleA, handleB, "the fixture must be able to tell the two apart")

	a, err := pi.CreatePipelineSession(ctx, "review", "agent/alpha", handleA)
	require.NoError(t, err)
	b, err := pi.CreatePipelineSession(ctx, "review", "agent/beta", handleB)
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID, "two distinct sessions")

	// The value the constructor hands back.
	require.Equal(t, handleA, a.CreatedBy)
	require.Equal(t, handleB, b.CreatedBy)

	// The value that survived to the row — the one a forensic query reads.
	gotA, err := pi.GetPipelineSession(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, gotA)
	gotB, err := pi.GetPipelineSession(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, gotB)

	require.Equal(t, handleA, gotA.CreatedBy, "session A must carry A's handle, not B's and not the first-written one")
	require.Equal(t, handleB, gotB.CreatedBy, "session B must carry B's handle")

	// Everything else on the row must be unaffected — the added column must not
	// have shifted the SELECT's scan order onto the wrong destinations, which is
	// a failure mode that leaves created_by itself looking perfectly correct.
	require.Equal(t, "agent/alpha", gotA.Branch)
	require.Equal(t, "review", gotA.Tool)
	require.Equal(t, "active", gotA.Status)
	require.Equal(t, "work", gotA.Phase)
	require.False(t, gotA.Scoped)
}

// An in-process caller — a test, or a local tool driving Reviewer directly —
// has no request to attribute the session to, and passes "". That is a
// legitimate, reachable state, so it is asserted rather than guarded against:
// the column is NOT NULL DEFAULT ” and empty means "the opening call carried
// no handle", never "unknown".
func TestCreatePipelineSession_EmptyCreatedByRoundTrips(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test", "")
	require.NoError(t, err)
	require.Empty(t, sess.CreatedBy)

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "", got.CreatedBy, "empty must survive as empty, not become a sentinel")
}
