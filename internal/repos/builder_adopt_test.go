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
	return m
}

// writeFactOn writes a parseable fact to branch on the default repo.
func writeFactOn(t *testing.T, m *Manager, branch, path, body string) {
	t.Helper()
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)
	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(), branch, path, adoptFact(body),
		"test: write "+path, "created")
	require.NoError(t, err)
}

// headBranch returns the branch the default repo's git HEAD points at.
func headBranch(t *testing.T, m *Manager) string {
	t.Helper()
	def, err := testService(t, m.Get(config.DefaultRepoName)).Branches().DefaultBranch(context.Background())
	require.NoError(t, err)
	return def
}

// TestEnsureBranch_AdoptsAgentBranchOnRestoredHome regression-tests issue #32:
// when a knomit home is restored onto a different machine — or a repo database
// is copied — the new instance looks for an agent branch that does not exist in
// that database. ensureBranch used to seed the branch from itself
// (CreateBranch(agent, agent)), which cannot succeed when the branch is absent,
// so the branch was never created and EVERY write path on that repo broke.
//
// The fix: when the configured agent branch is absent, adopt the repo's current
// HEAD branch (the previous machine's agent branch) as the seed source, the
// same way a fresh clone bootstraps its agent ref from origin/main.
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
	svc := testService(t, m2.Get(config.DefaultRepoName))

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
	rep, err := m2.Get(config.DefaultRepoName).Verify(context.Background(), store.VerifyOpts{Deep: true})
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

	// Adoption must also move HEAD onto the newly adopted branch — see
	// TestEnsureBranch_ChainedRestoreAdoptsMostRecentBranch for why.
	require.Equal(t, newAgent, headBranch(t, m2),
		"adoption must move HEAD to the adopted branch, not leave it on the orphan")
}

// TestOpenOne_EnsureBranchRunsBeforeLoadOntology pins the statement order in
// Manager.openOne. loadOntology reads — and may rewrite — domains/ontology.yaml
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
	_, err := testService(t, m1.Get(config.DefaultRepoName)).Facts().WriteFact(
		context.Background(), oldAgent, "domains/ontology.yaml", customOntologyYAML,
		"test: seed custom ontology", "updated")
	require.NoError(t, err)
	_ = m1.Close()

	m2 := bootHome(t, dir, newAgent)
	ri2 := m2.Get(config.DefaultRepoName)
	require.NotNil(t, ri2)
	require.NotNil(t, ri2.Ontology())
	require.Equal(t, "restored-home-custom", ri2.Ontology().ID,
		"restored home must load the adopted branch's ontology on the FIRST boot, not the default; issue #32")
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

	// Machine A creates the repo; HEAD is agentA.
	mA := bootHome(t, dir, agentA)
	writeFactOn(t, mA, agentA, "kb/notes/from-a.md", "written on the original machine")
	svcA := testService(t, mA.Get(config.DefaultRepoName))

	// Hand-build the damaged state: agentB exists (seeded from agentA) but HEAD
	// was never moved off agentA — what a failed SetDefaultBranch leaves behind.
	require.NoError(t, svcA.Branches().CreateBranch(context.Background(), agentB, agentA))
	require.Equal(t, agentA, headBranch(t, mA), "precondition: HEAD still on the orphan")
	_ = mA.Close()

	// Boot as machine B. No adoption fires — agentB already exists — so the
	// repair must come from the mismatch check, not the adoption path.
	mB := bootHome(t, dir, agentB)
	require.Equal(t, agentB, headBranch(t, mB),
		"HEAD must be repaired to this machine's agent branch even when no adoption fires")

	// Repairing HEAD must not disturb the data on either branch.
	svcB := testService(t, mB.Get(config.DefaultRepoName))
	for _, branch := range []string{agentA, agentB} {
		res, err := svcB.Facts().ReadFact(context.Background(), branch, "kb/notes/from-a.md", nil)
		require.NoError(t, err, "repairing HEAD must not disturb %s", branch)
		require.Contains(t, res.Content, "written on the original machine")
	}
}

// TestEnsureBranch_DetachedHeadDoesNotDangleHead pins the ordering guard in
// ensureBranch: HEAD is repaired only AFTER CreateBranch reports success.
//
// With a detached HEAD and the agent branch absent, seedSourceForAgentBranch
// has no source to adopt from, so it falls back to the agent branch itself and
// CreateBranch fails loudly — the same diagnostic as before the #32 fix. If the
// repair ran regardless, it would point HEAD at a branch that does not exist;
// go-git's repo.Head() cannot resolve a dangling HEAD, so the NEXT boot would
// fail to open the repo at all. Leaving HEAD detached is the safe outcome.
func TestEnsureBranch_DetachedHeadDoesNotDangleHead(t *testing.T) {
	dir := t.TempDir()

	const agentA = "agent/hosta-0badf00d"
	const agentB = "agent/hostb-cafebabe"

	mA := bootHome(t, dir, agentA)
	writeFactOn(t, mA, agentA, "kb/notes/from-a.md", "written on the original machine")
	tip, err := testService(t, mA.Get(config.DefaultRepoName)).Branches().HeadCommit(context.Background(), agentA)
	require.NoError(t, err)
	_ = mA.Close()

	// Detach HEAD directly in the ref store: point it at a raw commit hash
	// instead of a branch. DefaultBranch then reports "" (no symbolic target).
	dbPath := filepath.Join(dir, "repos", config.DefaultRepoName+".db")
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE refs SET target = ?, is_symbolic = 0 WHERE name = 'HEAD'`, tip)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Boot under an agent branch that does not exist. Adoption cannot fire (no
	// HEAD branch to adopt from) and CreateBranch fails, so HEAD must be left
	// exactly as it was rather than repointed at the missing branch.
	mB := bootHome(t, dir, agentB)
	require.Equal(t, "", headBranch(t, mB),
		"HEAD must stay detached when CreateBranch failed; pointing it at a missing "+
			"branch would dangle it and break the next OpenRepo")

	// The repo must still open on a subsequent boot — the actual harm a dangling
	// HEAD would cause.
	_ = mB.Close()
	mC := bootHome(t, dir, agentA)
	res, err := testService(t, mC.Get(config.DefaultRepoName)).Facts().
		ReadFact(context.Background(), agentA, "kb/notes/from-a.md", nil)
	require.NoError(t, err, "repo must still open and read after a failed adoption")
	require.Contains(t, res.Content, "written on the original machine")
}
