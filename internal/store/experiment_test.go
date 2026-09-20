package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// Experiments are forked from an AGENT branch, so these fixtures name the
// parent "agent/test" rather than "main" — the name is what CommitExperiment
// merges back into, and a fixture that used the consensus branch would be
// testing a shape production never produces.
const testAgentBranch = "agent/test"

// newExperimentTestStore returns a store whose consensus branch is "main" and
// whose agent branch "agent/test" holds one fact, so a fork has real history
// behind it rather than the root commit.
func newExperimentTestStore(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), testAgentBranch, "main"))
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "base body")
	return svc
}

// setExperimentActivity backdates a row so expiry and refresh can be asserted
// without sleeping or depending on the clock's resolution.
func setExperimentActivity(t *testing.T, svc *Service, name string, when time.Time) {
	t.Helper()
	res, err := svc.rh.db.Exec(
		`UPDATE experiments SET last_activity_at = ? WHERE name = ?`, when.Unix(), name)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "backdating must hit exactly the row %q", name)
}

func countRows(t *testing.T, svc *Service, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, svc.rh.db.QueryRow(query, args...).Scan(&n))
	return n
}

// readExperimentFact returns the fact's content on a branch, failing the test
// rather than making every caller unwrap the result struct.
func readExperimentFact(t *testing.T, svc *Service, branch, path string) string {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	return res.Content
}

// newTestSigner returns an ephemeral ed25519 SSH signer. Commit signing is a
// no-op without one, so any test that asserts a signature has to install it.
func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return signer
}

// TestOpenExperiment_ForksFromParentAndRecordsIt pins the two things that make
// an experiment an experiment: the ref exists at the parent's tip, and the
// parentage is RECORDED rather than inferred from the ref name
// (kb/invariants/store/branch-roles).
func TestOpenExperiment_ForksFromParentAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	parentHead, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)

	exp, err := svc.Experiments().OpenExperiment(ctx, "widen-gate", "trying a wider gate", testAgentBranch)
	require.NoError(t, err)

	require.Equal(t, "widen-gate", exp.Name)
	require.Equal(t, "trying a wider gate", exp.Description)
	require.Equal(t, testAgentBranch, exp.Parent, "parent must be the RECORDED fork source")
	require.Equal(t, parentHead, exp.ForkCommit, "fork commit must be the parent's tip at fork time")
	require.Equal(t, "exp/widen-gate", exp.Branch())

	expHead, err := svc.Branches().HeadCommit(ctx, "exp/widen-gate")
	require.NoError(t, err)
	require.Equal(t, parentHead, expHead, "the new ref must start at the parent's tip")

	// The row is the record — reading it back is what eligibility will do.
	got, ok, err := svc.Experiments().GetExperiment(ctx, "widen-gate")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, testAgentBranch, got.Parent)
	require.Equal(t, parentHead, got.ForkCommit)
}

// TestOpenExperiment_CopiesPipelineWatermarks is the falsifiable form of "a
// review on a fresh experiment seeds only the delta": CreateBranch
// deliberately does NOT copy pipeline_watermarks
// (kb/architecture/synthesize/pipeline-watermark), so without the fork's extra
// step the child is full-corpus dirty. Asserts WHICH value was copied, per
// tool, not merely that a row exists.
func TestOpenExperiment_CopiesPipelineWatermarks(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", testAgentBranch, "aaaa1111"))
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "hypothesize", testAgentBranch, "bbbb2222"))
	// A watermark on an unrelated branch must NOT be dragged along.
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", "main", "cccc3333"))

	_, err := svc.Experiments().OpenExperiment(ctx, "seeded", "", testAgentBranch)
	require.NoError(t, err)

	review, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "exp/seeded")
	require.NoError(t, err)
	require.Equal(t, "aaaa1111", review, "review watermark must be inherited from the parent, not main")

	hypo, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "exp/seeded")
	require.NoError(t, err)
	require.Equal(t, "bbbb2222", hypo, "every tool's watermark is inherited, not just review")

	require.Equal(t, 2, countRows(t, svc,
		`SELECT count(*) FROM pipeline_watermarks WHERE branch = ?`, "exp/seeded"),
		"exactly the parent's two rows, nothing from main")
}

// TestOpenExperiment_ResumesWithoutReforking: open is create-or-resume. A
// second open after the parent moved must NOT re-fork — that would silently
// discard the experiment's work.
func TestOpenExperiment_ResumesWithoutReforking(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	first, err := svc.Experiments().OpenExperiment(ctx, "resume-me", "first note", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/resume-me", "kb/only-here.md", "only here", "body")
	expHead, err := svc.Branches().HeadCommit(ctx, "exp/resume-me")
	require.NoError(t, err)

	// The parent moves on underneath the experiment.
	writeMergeFact(t, svc, testAgentBranch, "kb/moved-on.md", "moved on", "body")

	second, err := svc.Experiments().OpenExperiment(ctx, "resume-me", "second note", testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, first.ForkCommit, second.ForkCommit, "resume must not re-fork")
	require.Equal(t, "second note", second.Description, "resume updates the description")
	require.Equal(t, first.CreatedAt.Unix(), second.CreatedAt.Unix(), "created_at is set once")

	afterHead, err := svc.Branches().HeadCommit(ctx, "exp/resume-me")
	require.NoError(t, err)
	require.Equal(t, expHead, afterHead, "resume must leave the experiment's own tip alone")

	// An empty description on resume LEAVES the stored one — it is not a
	// silent erase of the note the agent wrote when it opened the experiment.
	third, err := svc.Experiments().OpenExperiment(ctx, "resume-me", "", testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, "second note", third.Description)
}

// TestOpenExperiment_RejectsBadNames: the name is kebab-case and unique per
// repo. Asserts the refusal happened BEFORE any ref was created — a rejected
// name that still leaves refs/heads/exp/<junk> behind is the failure that
// would not show up in the error.
func TestOpenExperiment_RejectsBadNames(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	for _, bad := range []string{"", "Widen-Gate", "widen gate", "widen_gate", "-widen", "widen-", "widen--gate", "exp/widen", "../escape"} {
		_, err := svc.Experiments().OpenExperiment(ctx, bad, "", testAgentBranch)
		require.Error(t, err, "name %q must be refused", bad)
		require.ErrorIs(t, err, ErrInvalidExperimentName, "name %q", bad)
	}
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM experiments`))
	branches, err := svc.Branches().ListBranches(ctx)
	require.NoError(t, err)
	for _, b := range branches {
		require.NotContains(t, b.Name, "exp/", "a refused name must create no branch, got %q", b.Name)
	}
}

// TestCommitExperiment_MergesAndDeletes: the happy path. The fact authored on
// the experiment reaches the parent, and the experiment is gone afterwards —
// ref, row, branch rows and watermark rows.
func TestCommitExperiment_MergesAndDeletes(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "landing", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/landing", "kb/from-exp.md", "from exp", "body")

	res, err := svc.Experiments().CommitExperiment(ctx, "landing")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode, "no parent movement since the fork: this is a fast-forward")

	content := readExperimentFact(t, svc, testAgentBranch, "kb/from-exp.md")
	require.Contains(t, content, "from exp", "the experiment's fact must be on the parent after commit")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "landing")
	require.NoError(t, err)
	require.False(t, ok, "the experiment row is gone after a successful commit")

	_, err = svc.Branches().HeadCommit(ctx, "exp/landing")
	require.Error(t, err, "the experiment ref is gone after a successful commit")

	require.Equal(t, 0, countRows(t, svc,
		`SELECT count(*) FROM pipeline_watermarks WHERE branch = ?`, "exp/landing"),
		"watermark rows are keyed by branch NAME with no FK, so commit must delete them itself")
}

// TestCommitExperiment_RefusesOnConflict is the blocking behaviour: the merge
// reports every conflicting path and changes NOTHING. A refuse that resolved
// the conflict silently would still return a plausible-looking success.
func TestCommitExperiment_RefusesOnConflict(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "clashing", "", testAgentBranch)
	require.NoError(t, err)

	// Both sides edit the SAME path relative to the fork point, plus one
	// non-conflicting file each, so the reported set must be exactly one path.
	writeMergeFact(t, svc, "exp/clashing", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, "exp/clashing", "kb/exp-only.md", "exp only", "body")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/agent-only.md", "agent only", "body")

	agentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	expBefore, err := svc.Branches().HeadCommit(ctx, "exp/clashing")
	require.NoError(t, err)
	commitsBefore := countRows(t, svc, `SELECT count(*) FROM branch_commits`)

	_, err = svc.Experiments().CommitExperiment(ctx, "clashing")
	require.Error(t, err)

	var conflict *MergeConflictError
	require.ErrorAs(t, err, &conflict, "the refusal must be a typed conflict error")
	require.Equal(t, []string{"kb/base.md"}, conflict.Paths,
		"exactly the conflicting path, not every path the merge looked at")

	agentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter, "a refused commit must not move the parent ref")
	expAfter, err := svc.Branches().HeadCommit(ctx, "exp/clashing")
	require.NoError(t, err)
	require.Equal(t, expBefore, expAfter, "a refused commit must not move the experiment ref")
	require.Equal(t, commitsBefore, countRows(t, svc, `SELECT count(*) FROM branch_commits`),
		"a refused commit must append no branch_commits row")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "clashing")
	require.NoError(t, err)
	require.True(t, ok, "a refused commit leaves the experiment in place")

	// The agent branch still holds ITS version — the refusal changed nothing.
	agentContent := readExperimentFact(t, svc, testAgentBranch, "kb/base.md")
	require.Contains(t, agentContent, "agent rewrite")
}

// TestSyncExperiment_AgentWinsThenCommitSucceeds is the only exit from a
// refused commit other than rollback: sync brings the parent's version of the
// conflicting path onto the experiment, and the next commit goes through.
func TestSyncExperiment_AgentWinsThenCommitSucceeds(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "reconciling", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/reconciling", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, "exp/reconciling", "kb/exp-only.md", "exp only", "kept through sync")
	writeMergeFact(t, svc, testAgentBranch, "kb/base.md", "base", "agent rewrite")

	_, err = svc.Experiments().CommitExperiment(ctx, "reconciling")
	require.Error(t, err, "precondition: the commit is refused")

	syncRes, err := svc.Experiments().SyncExperiment(ctx, "reconciling")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, syncRes.Mode)

	// Agent-branch-wins on the conflicting path...
	onExp := readExperimentFact(t, svc, "exp/reconciling", "kb/base.md")
	require.Contains(t, onExp, "agent rewrite",
		"sync is agent-branch-wins: the experiment must now carry the parent's version")
	// ...and the experiment's own non-conflicting work survives.
	kept := readExperimentFact(t, svc, "exp/reconciling", "kb/exp-only.md")
	require.Contains(t, kept, "kept through sync")

	_, err = svc.Experiments().CommitExperiment(ctx, "reconciling")
	require.NoError(t, err, "after sync there is no conflict left, so the commit lands")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "reconciling")
	require.NoError(t, err)
	require.False(t, ok)

	// The parent kept ITS version of the contested path — sync did not smuggle
	// the experiment's rewrite back in through the commit.
	final := readExperimentFact(t, svc, testAgentBranch, "kb/base.md")
	require.Contains(t, final, "agent rewrite")
	landed := readExperimentFact(t, svc, testAgentBranch, "kb/exp-only.md")
	require.Contains(t, landed, "kept through sync")
}

// TestRollbackExperiment_RemovesEveryTrace covers the reuse hazard: the row,
// the ref, the branch rows, AND the state keyed by branch NAME that DropBranch
// does not touch (pipeline_watermarks, the meta watermark keys). A name reused
// after a rollback must start from scratch.
func TestRollbackExperiment_RemovesEveryTrace(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", testAgentBranch, "aaaa1111"))
	_, err := svc.Experiments().OpenExperiment(ctx, "throwaway", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/throwaway", "kb/scratch.md", "scratch", "body")
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", "exp/throwaway", "dddd4444"))

	var branchID int64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, "exp/throwaway").Scan(&branchID))
	require.Positive(t, countRows(t, svc, `SELECT count(*) FROM branch_facts WHERE branch_id = ?`, branchID))

	require.NoError(t, svc.Experiments().RollbackExperiment(ctx, "throwaway"))

	_, ok, err := svc.Experiments().GetExperiment(ctx, "throwaway")
	require.NoError(t, err)
	require.False(t, ok, "the experiment row is gone")
	_, err = svc.Branches().HeadCommit(ctx, "exp/throwaway")
	require.Error(t, err, "the git ref is gone")
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM branches WHERE name = ?`, "exp/throwaway"))
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM branch_facts WHERE branch_id = ?`, branchID))
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM branch_commits WHERE branch_id = ?`, branchID))

	wm, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "exp/throwaway")
	require.NoError(t, err)
	require.Empty(t, wm,
		"pipeline_watermarks is keyed by branch NAME with no FK — rollback must delete it, "+
			"or a reused name inherits a stale watermark and its first review seeds nothing")
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM meta WHERE key LIKE ?`, "%:exp/throwaway"),
		"the branch-name-keyed meta watermarks go too")

	// The parent is untouched by a rollback.
	parentWM, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, "aaaa1111", parentWM)
}

// TestRollbackExperiment_ReusedNameForksClean is the consequence the previous
// test exists to protect: reopening the same name after a rollback gets the
// PARENT's watermark, not the dead experiment's.
func TestRollbackExperiment_ReusedNameForksClean(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", testAgentBranch, "aaaa1111"))
	_, err := svc.Experiments().OpenExperiment(ctx, "recycled", "", testAgentBranch)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", "exp/recycled", "dddd4444"))
	require.NoError(t, svc.Experiments().RollbackExperiment(ctx, "recycled"))

	_, err = svc.Experiments().OpenExperiment(ctx, "recycled", "", testAgentBranch)
	require.NoError(t, err)

	wm, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "exp/recycled")
	require.NoError(t, err)
	require.Equal(t, "aaaa1111", wm,
		"a reused name must inherit the PARENT's watermark, never the dead experiment's")
}

// TestExperimentCommits_AreSigned: signing is a Store-method-level guarantee
// (kb/decisions/git/commit-signing-always-on), and a new write path that
// synthesizes commits through plumbing would lose it silently.
//
// The fixture installs a signer explicitly, because signCommitInPlace is a
// NO-OP when none is set — without this the assertion would be about the
// fixture, and it would pass just as happily against a path that never called
// the signing helper at all. The guard below proves the signer is live before
// the experiment merge is judged by it.
func TestExperimentCommits_AreSigned(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	svc.SetSigner(newTestSigner(t))

	// Prove the signer is actually wired: an ordinary fact write must be
	// signed too, or a later empty signature says nothing about exp/*.
	guardHash := writeMergeFact(t, svc, testAgentBranch, "kb/guard.md", "guard", "body")
	guard, err := svc.rh.repo.CommitObject(plumbing.NewHash(guardHash))
	require.NoError(t, err)
	require.NotEmpty(t, guard.PGPSignature, "precondition: the test signer is installed")

	_, err = svc.Experiments().OpenExperiment(ctx, "signed", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/signed", "kb/base.md", "base", "experiment rewrite")
	writeMergeFact(t, svc, testAgentBranch, "kb/other.md", "other", "body")

	// A three-way merge onto the experiment (sync) synthesizes a commit —
	// that is the one this path adds, so that is the one to check.
	res, err := svc.Experiments().SyncExperiment(ctx, "signed")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "precondition: a real merge commit was synthesized")

	hash, err := svc.Branches().HeadCommit(ctx, "exp/signed")
	require.NoError(t, err)
	commit, err := svc.rh.repo.CommitObject(plumbing.NewHash(hash))
	require.NoError(t, err)
	require.NotEmpty(t, commit.PGPSignature, "the merge commit on exp/* must carry a signature")
}

// TestNotifyCommit_RefreshesExperimentActivity: activity is any commit on the
// experiment, recorded at the notifyCommit chokepoint. Backdated first so the
// assertion is about a VALUE that moved, not about clock resolution.
func TestNotifyCommit_RefreshesExperimentActivity(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "busy", "", testAgentBranch)
	require.NoError(t, err)
	old := time.Now().Add(-90 * 24 * time.Hour)
	setExperimentActivity(t, svc, "busy", old)

	writeMergeFact(t, svc, "exp/busy", "kb/new.md", "new", "body")

	got, ok, err := svc.Experiments().GetExperiment(ctx, "busy")
	require.NoError(t, err)
	require.True(t, ok)
	require.Greater(t, got.LastActivityAt.Unix(), old.Unix()+1,
		"a commit on the experiment refreshes last_activity_at")
	require.WithinDuration(t, time.Now(), got.LastActivityAt, time.Minute)
}

// TestNotifyCommit_ParentWriteDoesNotRefresh is the other half: a write on the
// agent branch is not the experiment's activity, so an abandoned experiment
// still ages out while its parent stays busy.
func TestNotifyCommit_ParentWriteDoesNotRefresh(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "idle", "", testAgentBranch)
	require.NoError(t, err)
	old := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	setExperimentActivity(t, svc, "idle", old)

	writeMergeFact(t, svc, testAgentBranch, "kb/parent-work.md", "parent work", "body")

	got, ok, err := svc.Experiments().GetExperiment(ctx, "idle")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, old.Unix(), got.LastActivityAt.Unix(),
		"a commit on the PARENT must not count as the experiment's activity")
}

// TestExpireExperiments_DropsOldKeepsYoung: the sweeper's store half. Asserts
// WHICH one went, and that the survivor is fully intact.
func TestExpireExperiments_DropsOldKeepsYoung(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "stale", "", testAgentBranch)
	require.NoError(t, err)
	_, err = svc.Experiments().OpenExperiment(ctx, "fresh", "", testAgentBranch)
	require.NoError(t, err)

	setExperimentActivity(t, svc, "stale", time.Now().Add(-40*24*time.Hour))
	setExperimentActivity(t, svc, "fresh", time.Now().Add(-2*24*time.Hour))

	dropped, err := svc.Experiments().ExpireExperiments(ctx, time.Now().Add(-30*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []string{"stale"}, dropped, "exactly the expired one")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "stale")
	require.NoError(t, err)
	require.False(t, ok)
	_, err = svc.Branches().HeadCommit(ctx, "exp/stale")
	require.Error(t, err, "expiry is a rollback: the ref goes too")

	_, ok, err = svc.Experiments().GetExperiment(ctx, "fresh")
	require.NoError(t, err)
	require.True(t, ok, "a younger experiment survives the same sweep")
	_, err = svc.Branches().HeadCommit(ctx, "exp/fresh")
	require.NoError(t, err)
}

// TestListExperiments_ReportsRecordedFields: the list is what the MCP tool and
// the REST route will render in PRs 2 and 3.
func TestListExperiments_ReportsRecordedFields(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "beta", "second", testAgentBranch)
	require.NoError(t, err)
	_, err = svc.Experiments().OpenExperiment(ctx, "alpha", "first", testAgentBranch)
	require.NoError(t, err)

	list, err := svc.Experiments().ListExperiments(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "alpha", list[0].Name, "listed by name so the order is stable")
	require.Equal(t, "first", list[0].Description)
	require.Equal(t, testAgentBranch, list[0].Parent)
	require.Equal(t, "beta", list[1].Name)
	require.Equal(t, "second", list[1].Description)
}

// TestExperimentOps_UnknownNameIsTyped: commit/sync/rollback of something that
// was never opened must say so, not fail deep inside the merge on a missing
// ref.
func TestExperimentOps_UnknownNameIsTyped(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().CommitExperiment(ctx, "ghost")
	require.ErrorIs(t, err, ErrNoSuchExperiment)
	_, err = svc.Experiments().SyncExperiment(ctx, "ghost")
	require.ErrorIs(t, err, ErrNoSuchExperiment)
	require.ErrorIs(t, svc.Experiments().RollbackExperiment(ctx, "ghost"), ErrNoSuchExperiment)
}

// TestReconcile_DoesNotTouchExperiments: an experiment is local work in
// progress and the consensus branch must never absorb it, nor the reconcile
// move its ref.
//
// The fixture has to CONTAIN an exp/* ref and the reconcile has to actually
// DO something in the same tick, or this is theatre: reconcile only ever
// names the agent branch and the upstream, so the guard is true by
// construction and an empty-repo version of this test passes against any
// implementation. The agent-tip assertion is what makes it reachable.
func TestReconcile_DoesNotTouchExperiments(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "in-progress", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/in-progress", "kb/wip.md", "wip", "not ready for anyone")
	expBefore, err := svc.Branches().HeadCommit(ctx, "exp/in-progress")
	require.NoError(t, err)

	// Give the reconcile real work: the agent branch must move THIS tick.
	writeMergeFact(t, svc, testAgentBranch, "kb/landed.md", "landed", "ready")
	agentTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	mainBefore, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, agentTip, mainBefore, "precondition: there is something to reconcile")

	res, err := svc.AdvanceLocalUpstream(ctx, testAgentBranch, "main")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode, "precondition: the tick did real work")

	mainAfter, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, agentTip, mainAfter, "the consensus branch DID move in this tick")

	expAfter, err := svc.Branches().HeadCommit(ctx, "exp/in-progress")
	require.NoError(t, err)
	require.Equal(t, expBefore, expAfter, "and the experiment's ref did not")

	// And the experiment's work did not leak into the consensus branch.
	_, err = svc.Facts().ReadFact(ctx, "main", "kb/wip.md", nil)
	require.Error(t, err, "an experiment's facts must not reach the consensus branch")
	landed, err := svc.Facts().ReadFact(ctx, "main", "kb/landed.md", nil)
	require.NoError(t, err)
	require.Contains(t, landed.Content, "ready")
}

// TestCommitExperiment_RefusesStaleParent: an experiment whose recorded
// parent is no longer this database's agent branch must not land. The binding
// gate one layer up already refuses such a write, but an IN-PROCESS caller
// does not pass through it, so the store checks the record too — the same
// two-layer shape as the subscription write refusal.
//
// Falsifiable on both tips: a merge that happened and was then reported as an
// error would move one of them.
func TestCommitExperiment_RefusesStaleParent(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	// Forked from agent/test, which this database owned at the time.
	_, err := svc.Experiments().OpenExperiment(ctx, "orphaned", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/orphaned", "kb/from-orphan.md", "from orphan", "body")

	// The database is taken over by a different agent branch.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/successor", testAgentBranch))
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, "agent/successor"))

	oldParentBefore, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	newOwnerBefore, err := svc.Branches().HeadCommit(ctx, "agent/successor")
	require.NoError(t, err)
	expBefore, err := svc.Branches().HeadCommit(ctx, "exp/orphaned")
	require.NoError(t, err)

	_, err = svc.Experiments().CommitExperiment(ctx, "orphaned")
	require.ErrorIs(t, err, ErrStaleExperimentParent)
	require.Contains(t, err.Error(), testAgentBranch, "the error names the recorded parent")
	require.Contains(t, err.Error(), "agent/successor", "and the branch that owns the database now")

	oldParentAfter, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, oldParentBefore, oldParentAfter, "the stale parent must not move")
	newOwnerAfter, err := svc.Branches().HeadCommit(ctx, "agent/successor")
	require.NoError(t, err)
	require.Equal(t, newOwnerBefore, newOwnerAfter, "and the current owner must not absorb it either")
	expAfter, err := svc.Branches().HeadCommit(ctx, "exp/orphaned")
	require.NoError(t, err)
	require.Equal(t, expBefore, expAfter, "the experiment is untouched")

	_, ok, err := svc.Experiments().GetExperiment(ctx, "orphaned")
	require.NoError(t, err)
	require.True(t, ok, "and still present — a refusal is not a rollback")
}

// TestSyncExperiment_RefusesStaleParent: the same gate on the other
// direction. Syncing FROM a branch this database no longer writes would pull
// another agent's consensus into a local experiment.
func TestSyncExperiment_RefusesStaleParent(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	_, err := svc.Experiments().OpenExperiment(ctx, "orphaned", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, testAgentBranch, "kb/moved-on.md", "moved on", "body")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/successor", testAgentBranch))
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, "agent/successor"))

	expBefore, err := svc.Branches().HeadCommit(ctx, "exp/orphaned")
	require.NoError(t, err)

	_, err = svc.Experiments().SyncExperiment(ctx, "orphaned")
	require.ErrorIs(t, err, ErrStaleExperimentParent)

	expAfter, err := svc.Branches().HeadCommit(ctx, "exp/orphaned")
	require.NoError(t, err)
	require.Equal(t, expBefore, expAfter, "a refused sync must not move the experiment")
}

// TestCommitExperiment_OwnerMatchesIsAllowed is the positive control: with the
// owner recorded and matching, the same commit lands. Without it the refusal
// test above would pass against an implementation that refused everything.
func TestCommitExperiment_OwnerMatchesIsAllowed(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	require.NoError(t, svc.Branches().SetAgentBranchOwner(ctx, testAgentBranch))

	_, err := svc.Experiments().OpenExperiment(ctx, "current", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/current", "kb/landing.md", "landing", "body")

	_, err = svc.Experiments().CommitExperiment(ctx, "current")
	require.NoError(t, err, "a recorded owner that MATCHES the parent must not block the commit")

	content := readExperimentFact(t, svc, testAgentBranch, "kb/landing.md")
	require.Contains(t, content, "landing")
}

// TestCommitExperiment_UnrecordedOwnerIsAllowed: AgentBranchOwner's contract
// says an empty value means UNKNOWN — a database created before the key
// existed, or one that has never completed a boot — and that callers must not
// read it as "no previous branch" (the same guard verify.go applies). Treating
// unknown as a mismatch would refuse every commit on such a database, so the
// check bites only on a KNOWN different owner.
func TestCommitExperiment_UnrecordedOwnerIsAllowed(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	owner, err := svc.Branches().AgentBranchOwner(ctx)
	require.NoError(t, err)
	require.Empty(t, owner, "fixture precondition: this database has no recorded owner")

	_, err = svc.Experiments().OpenExperiment(ctx, "unstamped", "", testAgentBranch)
	require.NoError(t, err)
	writeMergeFact(t, svc, "exp/unstamped", "kb/unstamped.md", "unstamped", "body")

	_, err = svc.Experiments().CommitExperiment(ctx, "unstamped")
	require.NoError(t, err, "an UNKNOWN owner is not a mismatch")
}

// TestOpenExperiment_RefusesOrphanRef: an exp/<name> ref with no experiments
// row is NOT adopted. It can exist — a prior open that died between
// CreateBranch and the INSERT, or a hand-made ref — and adopting it would
// hand the user a branch they believe is a fresh fork of the parent while it
// actually carries whatever that ref already pointed at. Every one of those
// commits then becomes committable into the agent branch under a name that
// says "new experiment".
func TestOpenExperiment_RefusesOrphanRef(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	// An orphan ref, forked from the parent at an OLD point...
	require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/orphan", testAgentBranch))
	writeMergeFact(t, svc, "exp/orphan", "kb/smuggled.md", "smuggled", "content nobody asked for")
	orphanTip, err := svc.Branches().HeadCommit(ctx, "exp/orphan")
	require.NoError(t, err)

	// ...and a parent that has since moved on.
	writeMergeFact(t, svc, testAgentBranch, "kb/newer.md", "newer", "body")

	_, err = svc.Experiments().OpenExperiment(ctx, "orphan", "", testAgentBranch)
	require.ErrorIs(t, err, ErrOrphanExperimentRef)
	require.Contains(t, err.Error(), "exp/orphan", "the error names the ref standing in the way")

	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM experiments WHERE name = ?`, "orphan"),
		"a refused open writes no row")
	after, err := svc.Branches().HeadCommit(ctx, "exp/orphan")
	require.NoError(t, err)
	require.Equal(t, orphanTip, after, "and does not touch the ref it refused")
}

// TestOpenExperiment_ForkCommitIsTheNewBranchTip closes the TOCTOU: the
// recorded fork point is read from the NEW branch after it exists, not from
// the parent before it does. Neither read holds the parent's lock, so a
// commit landing on the parent in between would otherwise leave fork_commit
// an ancestor of the branch's real starting point — and fork_commit is the
// only anchor a "changed since the fork" view has.
func TestOpenExperiment_ForkCommitIsTheNewBranchTip(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	exp, err := svc.Experiments().OpenExperiment(ctx, "anchored", "", testAgentBranch)
	require.NoError(t, err)

	branchTip, err := svc.Branches().HeadCommit(ctx, exp.Branch())
	require.NoError(t, err)
	require.Equal(t, branchTip, exp.ForkCommit,
		"the recorded fork point is the branch's own starting tip")

	parentTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, parentTip, exp.ForkCommit,
		"which, with nothing racing, is also the parent's tip at creation")

	// The stored row agrees with what the call returned.
	got, ok, err := svc.Experiments().GetExperiment(ctx, "anchored")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, branchTip, got.ForkCommit)
}

// TestRollbackExperiment_CleansAnOrphanRef: rollback is the RECOVERY for the
// state OpenExperiment refuses to adopt. An orphan ref — a prior open that
// died between CreateBranch and the INSERT — would otherwise be removable
// only from a shell, and the person hitting this in the UI does not have one.
func TestRollbackExperiment_CleansAnOrphanRef(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	// Exactly the shape a half-finished open leaves behind: the branch and
	// the copied watermarks exist, the experiments row does not.
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", testAgentBranch, "aaaa1111"))
	require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/half-open", testAgentBranch))
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", "exp/half-open", "aaaa1111"))
	writeMergeFact(t, svc, "exp/half-open", "kb/partial.md", "partial", "body")

	var branchID int64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, "exp/half-open").Scan(&branchID))
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM experiments WHERE name = ?`, "half-open"),
		"fixture precondition: there is no record, only a ref")

	require.NoError(t, svc.Experiments().RollbackExperiment(ctx, "half-open"),
		"rollback must clean an orphan ref rather than refuse it")

	_, err := svc.Branches().HeadCommit(ctx, "exp/half-open")
	require.Error(t, err, "the ref is gone")
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM branches WHERE name = ?`, "exp/half-open"))
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM branch_facts WHERE branch_id = ?`, branchID))
	wm, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "exp/half-open")
	require.NoError(t, err)
	require.Empty(t, wm, "the leftover watermark goes too — same cleanup as a recorded experiment")
	require.Equal(t, 0, countRows(t, svc, `SELECT count(*) FROM meta WHERE key LIKE ?`, "%:exp/half-open"))

	// The parent is untouched.
	parentWM, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, "aaaa1111", parentWM)
}

// TestRollbackExperiment_OrphanThenReopenSucceeds is the whole point of the
// previous test: the recovery has to leave the name USABLE, from the UI,
// without anyone touching a shell.
func TestRollbackExperiment_OrphanThenReopenSucceeds(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)

	require.NoError(t, svc.Branches().CreateBranch(ctx, "exp/retry-me", testAgentBranch))
	writeMergeFact(t, svc, "exp/retry-me", "kb/stale.md", "stale", "work from the dead open")

	// Refused while the orphan stands...
	_, err := svc.Experiments().OpenExperiment(ctx, "retry-me", "", testAgentBranch)
	require.ErrorIs(t, err, ErrOrphanExperimentRef)

	// ...rolled back, then it opens cleanly.
	require.NoError(t, svc.Experiments().RollbackExperiment(ctx, "retry-me"))

	parentTip, err := svc.Branches().HeadCommit(ctx, testAgentBranch)
	require.NoError(t, err)
	exp, err := svc.Experiments().OpenExperiment(ctx, "retry-me", "second try", testAgentBranch)
	require.NoError(t, err)
	require.Equal(t, parentTip, exp.ForkCommit, "the reopened experiment forks from the parent's tip")
	require.Equal(t, "second try", exp.Description)

	// And it does NOT carry the dead open's work.
	_, err = svc.Facts().ReadFact(ctx, exp.Branch(), "kb/stale.md", nil)
	require.Error(t, err, "the orphan's content must not survive into the fresh fork")
}

// TestRollbackExperiment_NeitherRefNorRowIsStillNotFound: the orphan path must
// not swallow the genuine not-found case into a silent success.
func TestRollbackExperiment_NeitherRefNorRowIsStillNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newExperimentTestStore(t)
	require.ErrorIs(t, svc.Experiments().RollbackExperiment(ctx, "nope"), ErrNoSuchExperiment)
}
