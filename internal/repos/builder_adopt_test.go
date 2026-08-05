package repos

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// The restored-home tests below all follow the same shape: boot a home under
// one agent branch, close it, and reopen it under a DIFFERENT one — the state a
// knomit home lands in when it is restored onto another machine. The agent
// branch name is agent/<hostname>-<key-fingerprint> (see app.agentBranch), so a
// fresh SSH key OR a changed hostname is enough to produce it.

// adoptFact wraps body in a well-formed fact. It parses, so writing it
// exercises the indexing path (branch_facts, facts_vec, the sync watermark)
// rather than being skipped as unparseable. Adoption clones branch_facts from
// the seed branch, so a fact that never got indexed leaves that clone untested.
func adoptFact(body string) string {
	return "---\ntype: observation\nconfidence: 0.5\nsources: 1\ndomain: [restore]\n" +
		"entities: []\nrefs: []\n---\n# fact\n\n" + body + "\n"
}

// bootHome starts a Manager against home dir under the given agent branch. The
// returned Manager is closed at test end; closing it earlier by hand is safe
// (Manager.Close is idempotent) and is how these tests simulate a machine
// shutting down before the home moves on.
func bootHome(t *testing.T, dir, agentBranch string) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	// First boot of a fresh home comes up with zero repos: Start creates
	// nothing. Later boots of the SAME home get the repo back from the registry,
	// which is the restore behaviour these tests are about — so only create when
	// it is genuinely absent.
	if m.Get(testRepoName) == nil {
		mustCreateRepo(t, m, testRepoName)
	}
	return m
}

// writeFactOn writes a parseable fact to branch on the test repo.
func writeFactOn(t *testing.T, m *Manager, branch, path, body string) {
	t.Helper()
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(), branch, path, adoptFact(body),
		"test: write "+path, "created")
	require.NoError(t, err)
}

// TestEnsureBranch_AdoptsAgentBranchOnRestoredHome regression-tests issue #32:
// when a knomit home is restored onto a different machine — or a repo database
// is copied — the new instance looks for an agent branch that does not exist in
// that database. ensureBranch used to seed the branch from itself
// (CreateBranch(agent, agent)), which cannot succeed when the branch is absent,
// so the branch was never created and EVERY write path on that repo broke.
//
// The fix: the database records which agent branch writes to it, and when the
// configured branch is absent the instance TAKES OVER — seeding the new branch
// from the recorded owner and then claiming ownership itself.
func TestEnsureBranch_AdoptsAgentBranchOnRestoredHome(t *testing.T) {
	dir := t.TempDir()

	const oldAgent = "agent/oldhost-0badf00d"
	const newAgent = "agent/newhost-cafebabe"

	// --- machine 1: accumulate knowledge on the old agent branch ---
	m1 := bootHome(t, dir, oldAgent)
	writeFactOn(t, m1, oldAgent, "kb/notes/local-only.md",
		"a fact that only ever lived on the previous machine's agent branch")
	_ = m1.Close()

	// --- machine 2: restore the same home under a DIFFERENT agent branch ---
	m2 := bootHome(t, dir, newAgent)
	svc := testService(t, m2.Get(testRepoName))

	// THE bug in #32: every write path was broken, not just index maintenance.
	// Assert it first so an unfixed tree reports the headline failure.
	_, err := svc.Facts().WriteFact(
		context.Background(), newAgent, "kb/notes/after-restore.md",
		adoptFact("written after the home was restored"),
		"test: write after restore", "created")
	require.NoError(t, err, "write path must work on the restored home; issue #32")

	head, err := svc.Branches().HeadCommit(context.Background(), newAgent)
	require.NoError(t, err, "new agent branch must exist after restore; issue #32")
	require.NotEmpty(t, head)

	// Adoption must preserve the previous machine's accumulated knowledge.
	res, err := svc.Facts().ReadFact(context.Background(), newAgent, "kb/notes/local-only.md", nil)
	require.NoError(t, err, "adopted branch must inherit the previous agent branch's facts")
	require.Contains(t, res.Content, "only ever lived on the previous machine")

	// The inherited fact must be INDEXED on the adopted branch, not merely
	// present in the tree — CreateBranch clones branch_facts from the seed, and
	// a gap there is exactly what Verify's facts-coherence check exists to catch.
	rep, err := m2.Get(testRepoName).Verify(context.Background(), store.VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, rep.IsClean(), "adopted branch must be index-coherent: %+v", rep.Issues)

	// The orphan is RETAINED, not garbage-collected (design decision: it may
	// hold the only copy of the previous machine's data). Nothing writes to it
	// and it is outside the fetch refspec, so keeping it is harmless.
	orphanHead, err := svc.Branches().HeadCommit(context.Background(), oldAgent)
	require.NoError(t, err, "the previous machine's agent branch must be retained, not GC'd")
	require.NotEmpty(t, orphanHead)
	orphanFact, err := svc.Facts().ReadFact(context.Background(), oldAgent, "kb/notes/local-only.md", nil)
	require.NoError(t, err, "the retained orphan must still carry its facts")
	require.Contains(t, orphanFact.Content, "only ever lived on the previous machine")

	// Taking over must also claim ownership, so the NEXT takeover seeds from
	// this branch rather than from the one it replaced — see
	// TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch.
	owner, err := svc.Branches().AgentBranchOwner(context.Background())
	require.NoError(t, err)
	require.Equal(t, newAgent, owner,
		"taking over must record the new agent branch as the repo's owner")
}

// TestOpenOne_EnsureBranchRunsBeforeLoadOntology pins the statement order in
// Manager.openOne. loadOntology reads — and may rewrite — the ontology file
// on the agent branch, so on a restored home it must run AFTER ensureBranch has
// adopted that branch. Running it first fell back to the default ontology and
// skipped the preset refresh on the first boot after a restore, self-correcting
// only on the next one.
func TestOpenOne_EnsureBranchRunsBeforeLoadOntology(t *testing.T) {
	dir := t.TempDir()

	const oldAgent = "agent/oldhost-0badf00d"
	const newAgent = "agent/newhost-cafebabe"

	// A unique id keeps this off the preset-refresh path, so the ontology the
	// restored instance loads is unambiguously the one written here.
	const customOntologyYAML = `id: restored-home-custom
name: Restored Home Custom
description: an ontology that only existed on the previous machine
topics:
  invariants:
    description: Load-bearing rules
`
	m1 := bootHome(t, dir, oldAgent)
	// Overwrite at the canonical path: the initial boot already seeded a
	// default ontology there, and the canonical path always wins over the
	// legacy one when both exist, so writing to the legacy path here would be
	// silently shadowed rather than exercising the restore scenario.
	_, err := testService(t, m1.Get(testRepoName)).Facts().WriteFact(
		context.Background(), oldAgent, OntologyPath, customOntologyYAML,
		"test: seed custom ontology", "updated")
	require.NoError(t, err)
	_ = m1.Close()

	m2 := bootHome(t, dir, newAgent)
	ri2 := m2.Get(testRepoName)
	require.NotNil(t, ri2)
	require.NotNil(t, ri2.Ontology())
	require.Equal(t, "restored-home-custom", ri2.Ontology().ID,
		"restored home must load the adopted branch's ontology on the FIRST boot, not the default; issue #32")
}

// TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch covers the SECOND hop
// of issue #32: a home that is restored, used, and then restored again.
//
// Each takeover claims ownership, so the recorded owner always names the branch
// most recently written to. Without that, the second restore would seed from
// whatever the first machine left behind and silently lose everything the
// intervening machine wrote — no error, no warning, the repo just comes up
// missing knowledge.
func TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch(t *testing.T) {
	dir := t.TempDir()

	const (
		agentA = "agent/hosta-0badf00d"
		agentB = "agent/hostb-cafebabe"
		agentC = "agent/hostc-deadbeef"
	)

	mA := bootHome(t, dir, agentA)
	writeFactOn(t, mA, agentA, "kb/notes/from-a.md", "written on the original machine")
	_ = mA.Close()

	// First restore: adopts A, then accumulates its own work.
	mB := bootHome(t, dir, agentB)
	writeFactOn(t, mB, agentB, "kb/notes/from-b.md", "written after the FIRST restore")
	_ = mB.Close()

	// Second restore: must adopt B (the most recent machine), not A.
	mC := bootHome(t, dir, agentC)
	svc := testService(t, mC.Get(testRepoName))
	for _, path := range []string{"kb/notes/from-a.md", "kb/notes/from-b.md"} {
		_, err := svc.Facts().ReadFact(context.Background(), agentC, path, nil)
		require.NoError(t, err,
			"a twice-restored home must inherit %s; each takeover must claim ownership "+
				"or the second one seeds from the original machine's branch and silently "+
				"drops the intervening machine's work (issue #32)", path)
	}
}

// TestEnsureBranch_DoesNotTakeOverAnotherAgentsBranch pins WHY the seed source
// is the recorded owner rather than HEAD.
//
// Several agents share a repo through its origin, and their agent branches are
// fetched into refs/heads — so a branch being present locally, or being what
// HEAD happens to point at, says nothing about whether THIS database writes to
// it. Seeding from HEAD would fork this instance off another agent's lineage:
// silent, and wrong in a way no restore scenario is involved in.
//
// Here HEAD is deliberately left on a foreign agent's branch. The takeover must
// still follow the ownership record.
func TestEnsureBranch_DoesNotTakeOverAnotherAgentsBranch(t *testing.T) {
	dir := t.TempDir()

	const (
		ours    = "agent/hosta-0badf00d"
		oursNew = "agent/hosta-cafebabe" // same machine, key regenerated
		foreign = "agent/otherhost-99999999"
	)

	mA := bootHome(t, dir, ours)
	writeFactOn(t, mA, ours, "kb/notes/from-us.md", "written by this database's agent")
	svcA := testService(t, mA.Get(testRepoName))

	// A second agent's branch, as a fetch from the shared origin would leave it:
	// present in refs/heads, carrying work that is not ours. Park HEAD on it.
	require.NoError(t, svcA.Branches().CreateBranch(context.Background(), foreign, ours))
	_, err := svcA.Facts().WriteFact(context.Background(), foreign, "kb/notes/from-them.md",
		adoptFact("written by a DIFFERENT agent sharing this origin"),
		"test: foreign agent write", "created")
	require.NoError(t, err)
	require.NoError(t, svcA.Branches().SetDefaultBranch(foreign))
	_ = mA.Close()

	// Reboot with a regenerated key. The takeover must seed from the recorded
	// owner (ours), not from HEAD (foreign).
	mB := bootHome(t, dir, oursNew)
	svcB := testService(t, mB.Get(testRepoName))

	res, err := svcB.Facts().ReadFact(context.Background(), oursNew, "kb/notes/from-us.md", nil)
	require.NoError(t, err, "takeover must inherit the recorded owner's work")
	require.Contains(t, res.Content, "written by this database's agent")

	_, err = svcB.Facts().ReadFact(context.Background(), oursNew, "kb/notes/from-them.md", nil)
	require.Error(t, err,
		"takeover must NOT inherit another agent's work; seeding from HEAD would "+
			"fork this instance off a lineage that is not this database's")
}

// TestEnsureBranch_NoRecordedOwnerFailsLoudly covers the one case the takeover
// deliberately does not handle: a database with no ownership record whose agent
// branch is already gone — a database that predates the record and lost its key
// before ever booting with it.
//
// There is no trustworthy seed source in that state, and guessing one is how a
// repo silently forks off the wrong lineage. Failing loudly is correct: origin
// is the source of truth, so re-cloning rebuilds the database. What must NOT
// happen is the repo becoming unopenable.
func TestEnsureBranch_NoRecordedOwnerFailsLoudly(t *testing.T) {
	dir := t.TempDir()

	const agentA = "agent/hosta-0badf00d"
	const agentB = "agent/hostb-cafebabe"

	mA := bootHome(t, dir, agentA)
	writeFactOn(t, mA, agentA, "kb/notes/from-a.md", "written on the original machine")
	_ = mA.Close()

	// Strip the ownership record to reproduce a pre-record database.
	dbPath := filepath.Join(dir, "repos", testRepoName+".db")
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM meta WHERE key = 'agent_branch_owner'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Boot under a branch that does not exist, with nothing recorded to seed
	// from. The branch must not be conjured out of some other lineage.
	mB := bootHome(t, dir, agentB)
	_, err = testService(t, mB.Get(testRepoName)).
		Branches().HeadCommit(context.Background(), agentB)
	require.Error(t, err,
		"with no recorded owner there is no trustworthy seed; the branch must not be created")
	_ = mB.Close()

	// The repo must still open and read on a later boot — a failed takeover must
	// not leave the database damaged.
	mC := bootHome(t, dir, agentA)
	res, err := testService(t, mC.Get(testRepoName)).Facts().
		ReadFact(context.Background(), agentA, "kb/notes/from-a.md", nil)
	require.NoError(t, err, "repo must still open and read after a failed takeover")
	require.Contains(t, res.Content, "written on the original machine")
}
