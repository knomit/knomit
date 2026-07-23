package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestEnsureBranch_AdoptsAgentBranchOnRestoredHome regression-tests issue #32:
// when a knomit home is restored onto a different machine — or a repo database
// is copied — the new instance generates a fresh SSH key, so its agent branch
// name (derived from the key fingerprint) does not exist in the copied
// database. ensureBranch used to seed the branch from itself
// (CreateBranch(agent, agent)), which cannot succeed when the branch is absent,
// so the branch was never created and EVERY write path on that repo broke.
//
// The fix: when the configured agent branch is absent, adopt the repo's current
// HEAD branch (the previous machine's agent branch) as the seed source, the
// same way a fresh clone bootstraps its agent ref from origin/main.
func TestEnsureBranch_AdoptsAgentBranchOnRestoredHome(t *testing.T) {
	dir := t.TempDir()

	// --- machine 1: boot the default repo under the old key's agent branch ---
	const oldAgent = "agent/oldhost-0badf00d"
	m1 := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: oldAgent,
	})
	require.NoError(t, m1.Start())
	t.Cleanup(func() { _ = m1.Close() })

	ri1 := m1.Get(config.DefaultRepoName)
	require.NotNil(t, ri1)

	// Accumulate local knowledge on the old agent branch so we can prove the
	// adopted branch inherits it (rather than seeding from an empty upstream).
	_, err := testService(t, ri1).Facts().WriteFact(
		context.Background(),
		oldAgent,
		"kb/notes/local-only.md",
		"a fact that only ever lived on the previous machine's agent branch",
		"test: seed local-only fact",
		"created",
	)
	require.NoError(t, err)

	// Give the old branch a custom ontology (unique id → not a preset, so the
	// refresh path leaves it untouched). ensureBranch must run before
	// loadOntology so the restored instance reads THIS ontology off the adopted
	// branch rather than falling back to the default on the first boot.
	const customOntologyYAML = `id: restored-home-custom
name: Restored Home Custom
description: an ontology that only existed on the previous machine
topics:
  invariants:
    description: Load-bearing rules
`
	_, err = testService(t, ri1).Facts().WriteFact(
		context.Background(),
		oldAgent,
		"domains/ontology.yaml",
		customOntologyYAML,
		"test: seed custom ontology",
		"updated",
	)
	require.NoError(t, err)
	require.NoError(t, m1.Close())

	// --- machine 2: restore the same home under a DIFFERENT agent branch ---
	// (fresh id_ed25519 → different fingerprint → different branch name).
	const newAgent = "agent/newhost-cafebabe"
	m2 := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: newAgent,
	})
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })

	ri2 := m2.Get(config.DefaultRepoName)
	require.NotNil(t, ri2)
	svc := testService(t, ri2)

	// ensureBranch runs before loadOntology, so the restored instance must load
	// the ontology off the adopted branch — not fall back to the default.
	require.NotNil(t, ri2.Ontology())
	require.Equal(t, "restored-home-custom", ri2.Ontology().ID,
		"restored home must load the adopted branch's ontology, not the default; issue #32")

	// The new agent branch must now exist (ensureBranch adopted it).
	head, err := svc.Branches().HeadCommit(context.Background(), newAgent)
	require.NoError(t, err, "new agent branch must exist after restore; issue #32")
	require.NotEmpty(t, head)

	// Adoption must preserve the previous machine's accumulated knowledge.
	res, err := svc.Facts().ReadFact(context.Background(), newAgent, "kb/notes/local-only.md", nil)
	require.NoError(t, err, "adopted branch must inherit the previous agent branch's facts")
	require.Contains(t, res.Content, "only ever lived on the previous machine")

	// The write path must work on the restored home — the real bug in #32.
	_, err = svc.Facts().WriteFact(
		context.Background(),
		newAgent,
		"kb/notes/after-restore.md",
		"a fact written by the new machine after the home was restored",
		"test: write after restore",
		"created",
	)
	require.NoError(t, err, "write path must work on the restored home; issue #32")

	// Adoption must also move HEAD onto the newly adopted branch — see
	// TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch for why.
	def, err := svc.Branches().DefaultBranch(context.Background())
	require.NoError(t, err)
	require.Equal(t, newAgent, def,
		"adoption must move HEAD to the adopted branch, not leave it on the orphan")
}

// TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch covers the SECOND hop
// of issue #32: a home that is restored, used, and then restored again.
//
// Adoption seeds from the repo's HEAD branch. HEAD is written only at init
// (store.InitRepo / InitFromRemote), so unless adoption moves it, it stays
// pinned to the branch of the machine that first created the repo. The second
// restore then adopts from THAT branch and silently loses everything the first
// restored machine wrote — no error, no warning, the repo just comes up
// missing knowledge.
//
// The fix: ensureBranch moves HEAD onto the adopted branch, so each restore
// chains off the most recent machine's work rather than the original's.
func TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch(t *testing.T) {
	dir := t.TempDir()

	boot := func(agent string) *Manager {
		t.Helper()
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: dir},
			AgentBranch: agent,
		})
		require.NoError(t, m.Start())
		t.Cleanup(func() { _ = m.Close() })
		return m
	}

	write := func(m *Manager, branch, path, content string) {
		t.Helper()
		ri := m.Get(config.DefaultRepoName)
		require.NotNil(t, ri)
		_, err := testService(t, ri).Facts().WriteFact(
			context.Background(), branch, path, content, "test: "+path, "created")
		require.NoError(t, err)
	}

	const (
		agentA = "agent/hosta-0badf00d"
		agentB = "agent/hostb-cafebabe"
		agentC = "agent/hostc-deadbeef"
	)

	// --- machine A: original home ---
	mA := boot(agentA)
	write(mA, agentA, "kb/notes/from-a.md", "written on the original machine")
	require.NoError(t, mA.Close())

	// --- machine B: first restore, adopts A, then accumulates its own work ---
	mB := boot(agentB)
	write(mB, agentB, "kb/notes/from-b.md", "written after the FIRST restore")
	require.NoError(t, mB.Close())

	// --- machine C: second restore, must adopt B (not A) ---
	mC := boot(agentC)
	svc := testService(t, mC.Get(config.DefaultRepoName))

	for _, path := range []string{"kb/notes/from-a.md", "kb/notes/from-b.md"} {
		_, err := svc.Facts().ReadFact(context.Background(), agentC, path, nil)
		require.NoError(t, err,
			"a twice-restored home must inherit %s; adoption seeds from HEAD, so HEAD "+
				"must follow each adoption or the second restore silently reverts to the "+
				"original machine's branch (issue #32)", path)
	}
}

// TestEnsureBranch_RepairsHeadWhenAgentBranchAlreadyExists covers the recovery
// case: the agent branch is already present, but HEAD points somewhere else.
//
// Every path that creates a knomit repo — store.InitRepo, InitFromRemote,
// initFromEmptyRemote, and the runtime lifecycle.initLocal / initClone that
// route through them — sets HEAD to the local agent branch. "HEAD is this
// machine's agent branch" is therefore an invariant, and a restored home
// violating it is exactly what issue #32 is about.
//
// Adoption alone does not restore the invariant: if SetDefaultBranch fails
// once (or the branch was adopted by a build that did not move HEAD at all),
// the agent branch exists on every later boot, so no adoption fires and HEAD
// stays wrong forever — re-arming the chained-restore data loss with nothing
// but a stale warn log to show for it. ensureBranch repairs the mismatch on
// any boot instead of only on the boot that adopts.
func TestEnsureBranch_RepairsHeadWhenAgentBranchAlreadyExists(t *testing.T) {
	dir := t.TempDir()

	const (
		agentA = "agent/hosta-0badf00d"
		agentB = "agent/hostb-cafebabe"
	)

	boot := func(agent string) *Manager {
		t.Helper()
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: dir},
			AgentBranch: agent,
		})
		require.NoError(t, m.Start())
		t.Cleanup(func() { _ = m.Close() })
		return m
	}

	// Machine A creates the repo; HEAD is agentA.
	mA := boot(agentA)
	svcA := testService(t, mA.Get(config.DefaultRepoName))
	_, err := svcA.Facts().WriteFact(context.Background(), agentA,
		"kb/notes/from-a.md", "written on the original machine", "test: a", "created")
	require.NoError(t, err)

	// Hand-build the damaged state: agentB exists (seeded from agentA) but HEAD
	// was never moved off agentA — what a failed SetDefaultBranch leaves behind.
	require.NoError(t, svcA.Branches().CreateBranch(context.Background(), agentB, agentA))
	def, err := svcA.Branches().DefaultBranch(context.Background())
	require.NoError(t, err)
	require.Equal(t, agentA, def, "precondition: HEAD still on the orphan")
	require.NoError(t, mA.Close())

	// Boot as machine B. No adoption fires — agentB already exists — so the
	// repair must come from the mismatch check, not the adoption path.
	mB := boot(agentB)
	svcB := testService(t, mB.Get(config.DefaultRepoName))
	def, err = svcB.Branches().DefaultBranch(context.Background())
	require.NoError(t, err)
	require.Equal(t, agentB, def,
		"HEAD must be repaired to this machine's agent branch even when no adoption fires")
}
