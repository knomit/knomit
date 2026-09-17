package repos

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedJob installs a job in the manager's registry directly, so ordering and
// TTL can be asserted without running (and waiting out) real creates.
func seedJob(m *Manager, id, name string, startedAt time.Time, finishedAt time.Time) *CreateJob {
	j := &CreateJob{
		id:        id,
		name:      name,
		mode:      "preset",
		done:      make(chan struct{}),
		startedAt: startedAt,
		state:     CreateRunning,
	}
	if !finishedAt.IsZero() {
		j.state = CreateDone
		j.finishedAt = finishedAt
		close(j.done)
	}
	m.createJobsMu.Lock()
	if m.createJobs == nil {
		m.createJobs = make(map[string]*CreateJob)
	}
	m.createJobs[id] = j
	m.createJobsMu.Unlock()
	return j
}

// The collection is newest first, and a finished job past CreateJobTTL is gone
// — the same reap the registration path runs, so listing cannot resurrect what
// polling has already forgotten.
func TestCreateJobs_NewestFirstAndReapsExpired(t *testing.T) {
	m := newTestManager(t)
	now := time.Now().UTC()

	seedJob(m, "old", "a", now.Add(-10*time.Minute), now.Add(-9*time.Minute))
	seedJob(m, "new", "b", now.Add(-1*time.Minute), time.Time{})
	seedJob(m, "mid", "c", now.Add(-5*time.Minute), now.Add(-4*time.Minute))
	seedJob(m, "expired", "d", now.Add(-3*CreateJobTTL), now.Add(-2*CreateJobTTL))

	got := m.CreateJobs()
	ids := make([]string, 0, len(got))
	for _, st := range got {
		ids = append(ids, st.ID)
	}
	require.Equal(t, []string{"new", "mid", "old"}, ids)

	_, ok := m.CreateJobByID("expired")
	require.False(t, ok, "the listing reaped it, so polling must not find it either")
}

// A RUNNING job is never reaped however old it is: its own deadline bounds it,
// and dropping it would lose the outcome a client is waiting for.
func TestCreateJobs_NeverReapsARunningJob(t *testing.T) {
	m := newTestManager(t)
	seedJob(m, "ancient", "a", time.Now().UTC().Add(-10*CreateJobTTL), time.Time{})

	got := m.CreateJobs()
	require.Len(t, got, 1)
	require.Equal(t, CreateRunning, got[0].State)
}

// Dismiss forgets a FINISHED job, refuses a running one, and reports an
// unknown id as unknown.
func TestDismissCreateJob(t *testing.T) {
	m := newTestManager(t)
	now := time.Now().UTC()
	seedJob(m, "finished", "a", now.Add(-time.Minute), now)
	seedJob(m, "running", "b", now, time.Time{})

	require.ErrorIs(t, m.DismissCreateJob("nosuch"), ErrCreateUnknown)

	require.ErrorIs(t, m.DismissCreateJob("running"), ErrCreateRunning)
	_, ok := m.CreateJobByID("running")
	require.True(t, ok, "a refused dismiss must not have removed it")

	require.NoError(t, m.DismissCreateJob("finished"))
	_, ok = m.CreateJobByID("finished")
	require.False(t, ok)
	require.Len(t, m.CreateJobs(), 1)

	// Dismissing twice is not idempotent-by-accident: the second call reports
	// the id as unknown, which is what a client racing itself should see.
	require.ErrorIs(t, m.DismissCreateJob("finished"), ErrCreateUnknown)
}
