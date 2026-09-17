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

// Two jobs started at the SAME INSTANT still come back in one fixed order, and
// the same one every call. The order must be TOTAL, not merely "newest first".
//
// The timestamps are seeded identical on purpose rather than by starting two
// creates and hoping the clock collides. That hope is what made the original
// defect invisible: StartedAt is time.Now(), nanosecond-resolution on Linux and
// macOS, the system clock's tick on Windows — so the unstable sort underneath
// this was fine on two platforms and a coin toss on the third, and the repo's
// first Windows CI run is what caught it. A test for a tie has to CONSTRUCT the
// tie; one that waits for the platform to produce it is testing the platform.
func TestCreateJobs_SameInstantIsATotalOrder(t *testing.T) {
	m := newTestManager(t)
	at := time.Now().UTC()

	// Seeded in the order that makes an unstable sort most likely to expose
	// itself: the id that must come FIRST is inserted second.
	seedJob(m, "b-second-id", "beta", at, time.Time{})
	seedJob(m, "a-first-id", "alpha", at, time.Time{})

	first := m.CreateJobs()
	ids := make([]string, 0, len(first))
	for _, st := range first {
		ids = append(ids, st.ID)
	}
	require.Equal(t, []string{"a-first-id", "b-second-id"}, ids,
		"equal StartedAt must fall back to id ascending")

	// Repeated calls over an unchanged manager must agree. CreateJobs builds
	// `out` by ranging a MAP, so its input order is randomised on every call;
	// without a total order this is where the disagreement shows up.
	for i := 0; i < 20; i++ {
		got := m.CreateJobs()
		require.Equal(t, ids[0], got[0].ID, "call %d disagreed with the first", i)
		require.Equal(t, ids[1], got[1].ID, "call %d disagreed with the first", i)
	}
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
