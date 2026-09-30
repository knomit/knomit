package repos

// Test seams for the F08 PR D mission e2e (mission_e2e_test.go). That test is
// in package repos_test because it needs the REAL MCP handlers as a script's
// host (internal/mcp imports this package, so an in-package test cannot); the
// seams below expose, to that test binary only, what the in-package tests
// reach directly: the dispatcher's clock, its run bookkeeping, and the
// consensus merger's kick and counters.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SetTriggerClockForTest installs now as every dispatcher's run clock (and so
// the sandbox's Date) for the rest of the test.
func SetTriggerClockForTest(t *testing.T, now func() time.Time) {
	t.Helper()
	setHooks(t, triggerHooks{now: now})
}

// QuiesceTriggersForTest waits until the dispatcher's last completed run
// read the agent branch's CURRENT head and its buffer is flushed. A script's
// write is the NEXT advance, so a run whose scripts wrote leaves the head
// ahead of the run, and this keeps waiting until the scripts stop writing.
// (Waiting for one head at a time can miss it: chained runs move the
// completed head on before a poll sees it.)
func (ri *RepoInstance) QuiesceTriggersForTest(t *testing.T) {
	t.Helper()
	require.NotNil(t, ri.triggers, "the repo must have a dispatcher")
	require.Eventually(t, func() bool {
		head := ri.agentHeadForTest(t)
		got, _ := ri.triggers.runSequence()
		return got == head && ri.triggers.flushed() && ri.agentHeadForTest(t) == head
	}, 30*time.Second, 10*time.Millisecond, "the triggers of %s never went quiet", ri.agentBranch)
}

// KickTriggersForTest delivers one kick (a tick, as far as the dispatcher can
// tell: every run sweeps the due facts at the run clock) and waits for it to
// complete and flush, then for the scripts it started to go quiet.
func (ri *RepoInstance) KickTriggersForTest(t *testing.T) {
	t.Helper()
	kickAndWait(t, ri)
	ri.QuiesceTriggersForTest(t)
}

func (ri *RepoInstance) agentHeadForTest(t *testing.T) string {
	t.Helper()
	h, err := testService(t, ri).Branches().HeadCommit(context.Background(), ri.agentBranch)
	require.NoError(t, err)
	return h
}

// ConsensusCountsForTest is the consensus merger's counters: merges written
// and merges refused (conflict, unrelated histories, no signer).
type ConsensusCountsForTest struct {
	Runs, Merges, Refused int
	Warnings              []string
}

// ConsensusForTest reads the merger's counters; ok is false when the repo
// has no merger.
func (ri *RepoInstance) ConsensusForTest() (c ConsensusCountsForTest, ok bool) {
	if ri.consensus == nil {
		return c, false
	}
	s := ri.consensus.snapshot()
	return ConsensusCountsForTest{Runs: s.Runs, Merges: s.Merges, Refused: s.Refused, Warnings: s.Warnings}, true
}

// SettleConsensusForTest waits for a merger run that STARTED after this call
// (cHost.settle's shape).
func (ri *RepoInstance) SettleConsensusForTest(t *testing.T) {
	t.Helper()
	require.NotNil(t, ri.consensus, "the repo must have a consensus merger")
	for i := 0; i < 2; i++ {
		before := ri.consensus.snapshot().Runs
		ri.consensus.kickNow()
		require.Eventually(t, func() bool { return ri.consensus.snapshot().Runs > before }, 10*time.Second, 5*time.Millisecond,
			"the consensus merger never ran")
	}
}
