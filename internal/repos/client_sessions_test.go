package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
)

// Start opens the client-session store over the same control.db handle the
// repo registry owns, wired to the [session] client_* thresholds, and the
// existing reaper tick purges through it.
func TestManagerStart_OpensClientSessionsStore(t *testing.T) {
	m := newTestManager(t)
	m.deps.Cfg.Session.ClientRetention = "1h"
	require.Nil(t, m.ClientSessions(), "store must be nil before Start")
	require.NoError(t, m.Start())

	s := m.ClientSessions()
	require.NotNil(t, s, "store must be open after Start")
	require.Equal(t, time.Hour, s.Policy().Retention, "policy must come from config")

	ctx := context.Background()
	now := time.Now()
	require.NoError(t, s.Touch(ctx, sessions.Observation{SessionID: "old", Now: now.Add(-2 * time.Hour)}))
	require.NoError(t, s.Touch(ctx, sessions.Observation{SessionID: "new", Now: now}))

	m.tickSessionReaper(ctx, sessionReaperConfig{})

	rows, err := s.List(ctx, sessions.Filter{Now: now, IncludeHidden: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "new", rows[0].ID)
}

// Close releases the store with the handle it borrows.
func TestManagerClose_DropsClientSessionsStore(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	require.NotNil(t, m.ClientSessions())
	require.NoError(t, m.Close())
	require.Nil(t, m.ClientSessions())
}

// A malformed threshold surfaces at boot, like the reaper's.
func TestManagerStart_RejectsMalformedClientThreshold(t *testing.T) {
	m := newTestManager(t)
	m.deps.Cfg.Session.ClientDeadAfter = "nope"
	require.Error(t, m.Start())
}
