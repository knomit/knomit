package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// openExperimentOn forks an experiment from parent on ri's live store and
// returns its branch name.
func openExperimentOn(t *testing.T, ri *RepoInstance, name, parent string) string {
	t.Helper()
	var branch string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		exp, err := svc.Experiments().OpenExperiment(context.Background(), name, "", parent)
		require.NoError(t, err)
		branch = exp.Branch()
	}))
	require.Equal(t, "exp/"+name, branch)
	return branch
}

// TestWritableBranch_ExperimentForkedFromOwnAgentBranch is the widening: an
// experiment is writable BECAUSE its recorded parent is this instance's agent
// branch, not because its name starts with exp/.
func TestWritableBranch_ExperimentForkedFromOwnAgentBranch(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()
	require.NotEmpty(t, agent, "fixture must have an agent branch")

	branch := openExperimentOn(t, ri, "mine", agent)

	require.True(t, ri.WritableBranch(branch),
		"an experiment forked from this instance's agent branch is writable")
	require.True(t, ri.WritableBranch(agent), "the agent branch stays writable")
}

// TestWritableBranch_RefusesExperimentWithForeignParent: eligibility reads the
// RECORD. An experiment forked from another branch is refused even though its
// ref sits in the same namespace — this is the case that separates "reads the
// record" from "matches the prefix".
func TestWritableBranch_RefusesExperimentWithForeignParent(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()

	// A branch that is NOT this instance's agent branch, standing in for
	// another machine's agent/* branch.
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/other", agent))
	}))
	foreign := openExperimentOn(t, ri, "theirs", "agent/other")

	require.False(t, ri.WritableBranch(foreign),
		"an experiment whose recorded parent is not this instance's agent branch is NOT writable")
}

// TestWritableBranch_RefusesUnrecordedExperimentRef: a bare exp/* ref with no
// row is refused. Without this, anything that could create a ref in the
// namespace would grant itself write access.
func TestWritableBranch_RefusesUnrecordedExperimentRef(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), "exp/no-record", agent))
	}))

	require.False(t, ri.WritableBranch("exp/no-record"),
		"eligibility comes from the experiments RECORD, never from the ref name")
}

// TestWritableBranch_SubscriptionRefusesEveryExperiment is the quiet failure
// the widening could introduce. A subscription has agentBranch == ""
// (kb/invariants/repos/subscription/flag-and-branch-paired), so a comparison
// of "recorded parent == agent branch" with no non-empty guard makes
// "" == "" true and every exp/* branch writable on a repo that accepts no
// writes at all.
func TestWritableBranch_SubscriptionRefusesEveryExperiment(t *testing.T) {
	sub := NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "followed", Subscribed: true, ReadBranch: "main",
	})
	require.Empty(t, sub.AgentBranch(), "fixture precondition: a subscription has no agent branch")

	require.False(t, sub.WritableBranch("exp/anything"),
		"a subscription accepts no writes on ANY branch, experiments included")
	require.False(t, sub.WritableBranch(""), "the empty branch is not an accidental match either")
	require.False(t, sub.WritableBranch("main"))
}

// TestWritableBranch_OntologyLessRepoRefusesExperiments: the ontology gate
// covers the widened classification too. A repo that cannot validate a fact
// must not gain a back door through the experiment namespace.
func TestWritableBranch_OntologyLessRepoRefusesExperiments(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()
	branch := openExperimentOn(t, ri, "mine", agent)
	require.True(t, ri.WritableBranch(branch), "precondition: writable while the ontology is fine")

	ri.ontologyErr = errors.New("ontology could not be established")

	require.False(t, ri.WritableBranch(branch),
		"no ontology, no writes — on an experiment exactly as on the agent branch")
	require.False(t, ri.WritableBranch(agent))
}

// TestWritableBranch_ConsensusAndForeignAgentStayRefused pins the two classes
// the widening must NOT touch.
func TestWritableBranch_ConsensusAndForeignAgentStayRefused(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")

	require.False(t, ri.WritableBranch("main"), "the consensus branch is never writable")
	require.False(t, ri.WritableBranch("agent/someone-else"), "a foreign agent branch is never writable")
	require.False(t, ri.WritableBranch(""), "the empty branch is never writable")
}

// TestWriteBranch_ExperimentBindingTargetsTheExperiment: the binding answers
// WHERE a write lands. Bound to an experiment it is the experiment; bound to
// the agent branch it is the agent branch; on a read-only view it is empty, so
// a caller that skipped the WriteOK gate cannot get a plausible branch name
// out of it.
func TestWriteBranch_ExperimentBindingTargetsTheExperiment(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()
	branch := openExperimentOn(t, ri, "mine", agent)

	onExp := NewBindingOfRepo(ri, branch)
	require.True(t, onExp.WriteOK())
	require.Equal(t, branch, onExp.WriteBranch(), "writes land on the bound experiment")
	require.Equal(t, branch, onExp.WriteMountBranch(), "and reads come from the same place")

	onAgent := NewBindingOfRepo(ri, agent)
	require.True(t, onAgent.WriteOK())
	require.Equal(t, agent, onAgent.WriteBranch(), "unchanged for an ordinary agent-branch binding")

	onMain := NewBindingOfRepo(ri, "main")
	require.False(t, onMain.WriteOK())
	require.Empty(t, onMain.WriteBranch(), "a read-only view names no write branch")
}

// TestWriteBranch_ForeignExperimentIsReadOnly: bound to an experiment this
// instance may not write, the binding is a read-only view — it does not
// quietly redirect the write to the agent branch.
func TestWriteBranch_ForeignExperimentIsReadOnly(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/other", agent))
	}))
	foreign := openExperimentOn(t, ri, "theirs", "agent/other")

	b := NewBindingOfRepo(ri, foreign)
	require.False(t, b.WriteOK())
	require.Empty(t, b.WriteBranch())
}

// TestNewBindingOfLens_PinnedAtMainStaysWritable is the regression guard for
// the change to lens writeOK. A lens may pin its write member at the
// consensus branch FOR READS and still write to that member's agent branch —
// that is today's behaviour and PR 1 must not silently turn such a lens
// read-only.
//
// The pin has to be EXPLICIT in Reads. Lens.normalize appends the write member
// with an empty branch only when it is not already present, and an empty
// branch resolves to the member's read branch — i.e. its agent branch — so a
// lens built without an explicit write-member entry is pinned at the agent
// branch and this test would pass for the wrong reason. The
// WriteMountBranch assertion below is what proves the pin actually took.
func TestNewBindingOfLens_PinnedAtMainStaysWritable(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, "core")
	work := createRepo(t, m, "work")

	lens, err := m.LensRegistry().Create(Lens{
		Name:     "pinned",
		WriteUID: work.UID(),
		Reads: []LensRead{
			{RepoUID: work.UID(), Branch: "main"},
			{RepoUID: core.UID()},
		},
	})
	require.NoError(t, err)

	b, err := NewBindingOfLens(m, lens)
	require.NoError(t, err)
	require.Equal(t, "main", b.WriteMountBranch(),
		"precondition: the explicit pin survived normalize, so this test is reachable")
	require.True(t, b.WriteOK(),
		"a lens whose write member is pinned at main for READS still writes to its agent branch")
	require.Equal(t, work.AgentBranch(), b.WriteBranch(),
		"and the write lands on the agent branch, not on the read pin")
}

// TestNewBindingOfLens_PinnedAtExperimentWritesThere is the other direction:
// when the write member IS pinned at an experiment it may write, that is where
// the writes go. (PR 2 installs the re-pin from an active experiment; PR 1
// only has to be able to express it.)
func TestNewBindingOfLens_PinnedAtExperimentWritesThere(t *testing.T) {
	m := newLifecycleManager(t)
	core := createRepo(t, m, "core")
	work := createRepo(t, m, "work")
	branch := openExperimentOn(t, work, "lens-exp", work.AgentBranch())

	lens, err := m.LensRegistry().Create(Lens{
		Name:     "experimenting",
		WriteUID: work.UID(),
		Reads: []LensRead{
			{RepoUID: work.UID(), Branch: branch},
			{RepoUID: core.UID()},
		},
	})
	require.NoError(t, err)

	b, err := NewBindingOfLens(m, lens)
	require.NoError(t, err)
	require.Equal(t, branch, b.WriteMountBranch(), "precondition: pinned at the experiment")
	require.True(t, b.WriteOK())
	require.Equal(t, branch, b.WriteBranch(), "the write member writes to the experiment it is pinned at")

	// Every OTHER mount is untouched by the write member's experiment.
	for _, rt := range b.Reads() {
		if rt.RI == core {
			require.Equal(t, core.AgentBranch(), rt.Branch,
				"a co-mounted repo keeps its own branch")
		}
	}
}

// TestNewBindingOfLens_PinnedAtForeignExperimentFallsBack: pinned at an
// experiment this repo may NOT write, the lens behaves exactly as it does when
// pinned at any other unwritable branch — writes go to the agent branch.
func TestNewBindingOfLens_PinnedAtForeignExperimentFallsBack(t *testing.T) {
	m := newLifecycleManager(t)
	work := createRepo(t, m, "work")
	require.NoError(t, work.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/other", work.AgentBranch()))
	}))
	foreign := openExperimentOn(t, work, "theirs", "agent/other")

	lens, err := m.LensRegistry().Create(Lens{
		Name:     "stale-pin",
		WriteUID: work.UID(),
		Reads:    []LensRead{{RepoUID: work.UID(), Branch: foreign}},
	})
	require.NoError(t, err)

	b, err := NewBindingOfLens(m, lens)
	require.NoError(t, err)
	require.Equal(t, foreign, b.WriteMountBranch(), "precondition: pinned at the foreign experiment")
	require.True(t, b.WriteOK(), "unchanged from an unwritable pin of any other shape")
	require.Equal(t, work.AgentBranch(), b.WriteBranch())
}
