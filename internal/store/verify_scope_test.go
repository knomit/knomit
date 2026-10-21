package store

import (
	"context"
	"path/filepath"
	"testing"

	"knomit/internal/store/fsck"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// newVerifyRepo opens a store with one fact on the agent branch — the smallest
// repo that has real commits, tree entries and index rows to be wrong about.
func newVerifyRepo(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", "kb/decisions/x/aaaaaaaa.md",
		testFactBody("A", 0.9, nil), "seed", "learn")
	require.NoError(t, err)
	return svc
}

func categories(report fsck.IntegrityReport, cat string) []fsck.IntegrityIssue {
	var out []fsck.IntegrityIssue
	for _, i := range report.Issues {
		if i.Category == cat {
			out = append(out, i)
		}
	}
	return out
}

// The regression this whole pass exists for. A branch the index does not
// maintain has no branch_commits, no branch_facts and no branches row, all
// correctly — and Verify used to report one ERROR per commit and per fact on
// it. On a live five-repo home that was 8128 errors, every one false, and the
// tool exited 1 forever.
//
// The branch here is created directly through the storer, NOT via CreateBranch:
// a fetched foreign agent branch arrives as a bare ref with no index behind it,
// and that is the state being pinned.
func TestVerify_UnindexedBranchIsSkippedNotReported(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	foreign := plumbing.NewBranchReferenceName("agent/other-machine-deadbeef")
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(foreign, head.Hash())))

	report, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)

	require.Contains(t, report.Skipped, "agent/other-machine-deadbeef",
		"an unindexed ref must be reported as skipped, not silently dropped")
	require.NotContains(t, report.Branches, "agent/other-machine-deadbeef")
	require.True(t, report.IsClean(), "an unindexed branch must not produce errors: %v", report.Issues)
	for _, i := range report.Issues {
		require.NotEqual(t, "agent/other-machine-deadbeef", i.Branch,
			"no issue may be raised against a branch that was never parity-checked")
	}
}

// The escape hatch has to actually check what the default skips, or it is
// decoration. With AllBranches the same unindexed ref DOES produce errors.
func TestVerify_AllBranchesOptsBackIn(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("agent/other-machine-deadbeef"), head.Hash())))

	report, err := svc.Verify(ctx, fsck.VerifyOpts{AllBranches: true})
	require.NoError(t, err)
	require.Contains(t, report.Branches, "agent/other-machine-deadbeef")
	require.Empty(t, report.Skipped)
	require.False(t, report.IsClean(), "--all-branches must surface the parity gaps the default hides")
}

// --all-branches moves generated refs into the checked set, where they produce
// hard parity errors. The warning naming --prune-generated-refs must survive
// that, or the one hint pointing at the repair disappears exactly when an
// operator opts in to ask "why does this branch have no rows".
func TestVerify_AllBranchesStillNamesTheGeneratedRefRepair(t *testing.T) {
	svc := newVerifyRepo(t)

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("okf/main"), head.Hash())))

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{AllBranches: true})
	require.NoError(t, err)

	found := categories(report, fsck.CategoryGeneratedRefs)
	require.Len(t, found, 1, "the generated-ref warning must not be suppressed by --all-branches")
	require.Contains(t, found[0].Detail, "--prune-generated-refs")
}

// A brand-new repo has NO meta rows at all — the first last_commit:<branch> row
// appears only after the first write. So the maintained-branch oracle cannot be
// last_commit alone, or Verify checks nothing on a fresh repo and calls it
// CLEAN. That silent skip is the same defect as the false positives, pointing
// the other way.
func TestVerify_FreshRepoStillChecksItsAgentBranch(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))

	var metaRows int
	require.NoError(t, svc.rh.Storer().DB().QueryRow(
		`SELECT COUNT(*) FROM meta WHERE key LIKE 'last_commit:%'`).Scan(&metaRows))
	require.Zero(t, metaRows, "precondition: a fresh repo has no last_commit row")

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.Contains(t, report.Branches, "agent/test",
		"the agent branch must be checked even before the index has ever synced it")
}

// Generated okf/* refs are residue from the server-side export removed in
// bf9becbe. They are warned about (not errored on, so they cannot fail a run)
// and the warning names the repair.
func TestVerify_GeneratedRefIsWarnedNotErrored(t *testing.T) {
	svc := newVerifyRepo(t)

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("okf/main"), head.Hash())))

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "generated refs must not fail the run: %v", report.Issues)

	found := categories(report, fsck.CategoryGeneratedRefs)
	require.Len(t, found, 1)
	require.Equal(t, fsck.SeverityWarning, found[0].Severity)
	require.Equal(t, "okf/main", found[0].Branch)
	require.Contains(t, found[0].Detail, "--prune-generated-refs")
}

// The repair removes exactly the generated refs and their markers, and nothing
// else — a prune that took a real branch with it would be far worse than the
// residue it cleans up.
//
// SCOPE NOTE: the fixture points okf/* at the SAME commit as agent/test, so no
// object becomes unreachable here. That is not an accident and it is not full
// coverage: on a real home the generated refs hold their own export commits,
// and pruning them RAISES the orphan-objects count (2805 → 8378 on knomit-kb).
// That is expected — see checkOrphanObjects — and it is why orphan-objects is a
// warning and a count. Do not read this test as evidence that a prune leaves
// the object store unchanged.
func TestPruneGeneratedRefs_RemovesResidueAndNothingElse(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	for _, name := range []string{"okf/main", "okf/agent/test"} {
		require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
			plumbing.NewBranchReferenceName(name), head.Hash())))
	}
	_, err = svc.rh.Storer().DB().Exec(
		`INSERT OR REPLACE INTO kv(key, value) VALUES ('okf:marker:main', 'x')`)
	require.NoError(t, err)

	pruned, err := svc.PruneGeneratedRefs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"okf/main", "okf/agent/test"}, pruned.Refs)
	require.EqualValues(t, 1, pruned.Markers, "the marker row must be counted, not silently deleted")

	// The real branches survive.
	_, err = svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err, "prune must not touch a real branch")
	_, err = svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("main"))
	require.NoError(t, err, "prune must not touch main")

	// The generated ones are gone, markers included.
	for _, name := range []string{"okf/main", "okf/agent/test"} {
		_, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName(name))
		require.Error(t, err, "%s must be gone", name)
	}
	var markers int
	require.NoError(t, svc.rh.Storer().DB().QueryRow(
		`SELECT COUNT(*) FROM kv WHERE key LIKE 'okf:marker:%'`).Scan(&markers))
	require.Zero(t, markers)

	report, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)
	require.Empty(t, categories(report, fsck.CategoryGeneratedRefs),
		"after the prune there is nothing left to warn about")
}

// commit_parents is the third leg of the commit index and had no coverage:
// commit-log parity reads branch_commits and nothing read the DAG edges, so a
// history walk could take the wrong shape unnoticed.
func TestVerify_DetectsCommitParentsDrift(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	res, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/decisions/x/bbbbbbbb.md",
		testFactBody("B", 0.9, nil), "second", "learn")
	require.NoError(t, err)

	_, err = svc.rh.Storer().DB().Exec(
		`UPDATE commit_parents SET parent_hash = ? WHERE commit_hash = ?`,
		"0000000000000000000000000000000000000000", res.CommitHash)
	require.NoError(t, err)

	report, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)
	found := categories(report, fsck.CategoryCommitParents)
	require.NotEmpty(t, found, "a rewritten parent edge must be reported: %v", report.Issues)
	require.Equal(t, fsck.SeverityError, found[0].Severity)
}

// An unreadable commit_parents table must produce ONE issue, not one per
// commit. Comparing every commit against an empty map makes each non-root
// commit look like it lost its parent edges, so a single transient query error
// would manufacture thousands of false errors and a permanent exit 1 — the
// exact failure this whole change exists to remove, coming back in through the
// error path.
func TestVerify_UnreadableCommitParentsDoesNotFabricateOneErrorPerCommit(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	for _, p := range []string{"kb/decisions/x/bbbbbbbb.md", "kb/decisions/x/cccccccc.md"} {
		_, err := svc.Facts().WriteFact(ctx, "agent/test", p, testFactBody("B", 0.9, nil), "more", "learn")
		require.NoError(t, err)
	}

	// Precondition: several commits with real parent edges.
	var edges int
	require.NoError(t, svc.rh.Storer().DB().QueryRow(`SELECT COUNT(*) FROM commit_parents`).Scan(&edges))
	require.Greater(t, edges, 1)

	_, err := svc.rh.Storer().DB().Exec(`DROP TABLE commit_parents`)
	require.NoError(t, err)

	report, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)

	found := categories(report, fsck.CategoryCommitParents)
	require.Len(t, found, 1, "an unreadable table is ONE finding, not one per commit: %v", found)
	require.Contains(t, found[0].Detail, "read commit_parents")
	require.Contains(t, found[0].Detail, "skipped")
}

// A root commit has no parents and therefore no commit_parents rows. Absence is
// correct there, and reporting it would make every repo dirty from birth.
func TestVerify_RootCommitWithoutParentRowsIsClean(t *testing.T) {
	svc := newVerifyRepo(t)
	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.Empty(t, categories(report, fsck.CategoryCommitParents),
		"a root commit's missing parent rows are correct, not drift")
}

// The category is called commit-log and, since parity moved onto
// branch_commits, nothing read commit_log at all. Orphans are the direction
// with no legitimate exception.
func TestVerify_DetectsCommitLogOrphan(t *testing.T) {
	svc := newVerifyRepo(t)

	_, err := svc.rh.Storer().DB().Exec(
		`INSERT INTO commit_log(commit_hash, path, committed_at, message)
		 VALUES ('deadbeefdeadbeefdeadbeefdeadbeefdeadbeef', 'kb/ghost.md', 0, 'ghost')`)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	found := categories(report, fsck.CategoryCommitLog)
	require.NotEmpty(t, found, "a commit_log row for a nonexistent commit must be reported")
	require.Equal(t, fsck.SeverityError, found[0].Severity)
}

// A no-op commit (a write whose blob equals the parent's at the same path) has
// zero commit_log rows by design. That is exactly why parity moved off
// commit_log, and the orphan check must not reintroduce the false positive by
// checking the other direction.
func TestVerify_CommitWithoutCommitLogRowsIsClean(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	body := testFactBody("A", 0.9, nil)
	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/decisions/x/cccccccc.md", body, "first", "learn")
	require.NoError(t, err)
	// Same content at the same path: no tree change, so no commit_log rows.
	_, err = svc.Facts().WriteFact(ctx, "agent/test", "kb/decisions/x/cccccccc.md", body, "again", "learn")
	require.NoError(t, err)

	report, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "a no-op commit must not be reported: %v", report.Issues)
}

// The fact_* projections are what domain and entity search read, and their
// failure mode is silent: every fact present and correct, and searching by
// domain returns nothing.
// The orphan is inserted with foreign keys OFF because the connection enforces
// them, which is precisely why this check is defensive rather than routine: the
// rows it looks for cannot be produced by normal operation, only by a database
// written or restored without enforcement. That is worth checking and is not
// worth pretending is common.
func TestVerify_DetectsDerivedTableOrphan(t *testing.T) {
	svc := newVerifyRepo(t)

	_, err := svc.rh.Storer().DB().Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = svc.rh.Storer().DB().Exec(
		`INSERT INTO fact_domains(fact_id, domain) VALUES (999999, 'ghost')`)
	require.NoError(t, err)
	_, err = svc.rh.Storer().DB().Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	found := categories(report, fsck.CategoryDerivedTables)
	require.NotEmpty(t, found, "an orphan fact_domains row must be reported")
	require.Equal(t, fsck.SeverityError, found[0].Severity)
}

// Both writers insert canonicalizeDomain(d) into fact_domains AND
// fact_domain_tokens, so the strings "must" match — and on a live repo they do
// not, because canonicalizeDomain gained a de-hyphenize step and only the token
// side was repopulated. That skew is a WARNING: calling it corruption would
// report a healthy repo as broken, which is the bug this pass removes.
func TestVerify_DomainCanonicalizationSkewIsWarningOnly(t *testing.T) {
	svc := newVerifyRepo(t)

	var factID int64
	require.NoError(t, svc.rh.Storer().DB().QueryRow(`SELECT id FROM facts LIMIT 1`).Scan(&factID))
	_, err := svc.rh.Storer().DB().Exec(
		`INSERT OR REPLACE INTO fact_domains(fact_id, domain) VALUES (?, 'claude-code')`, factID)
	require.NoError(t, err)
	_, err = svc.rh.Storer().DB().Exec(
		`INSERT OR REPLACE INTO fact_domain_tokens(fact_id, domain, token) VALUES (?, 'claude code', 'claude')`,
		factID)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.True(t, report.IsClean(),
		"a canonicalization skew must not fail the run: %v", report.Issues)
	found := categories(report, fsck.CategoryDerivedTables)
	require.NotEmpty(t, found)
	require.Equal(t, fsck.SeverityWarning, found[0].Severity)
	require.Contains(t, found[0].Detail, "canonicalizeDomain drift")
}

// A database left mid-migration is neither the old schema nor the new one, and
// every other check is reading it as though it were one of the two.
func TestVerify_DetectsDirtySchemaVersion(t *testing.T) {
	svc := newVerifyRepo(t)

	_, err := svc.rh.Storer().DB().Exec(`UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, err)

	report, err := svc.Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	found := categories(report, fsck.CategorySchemaVersion)
	require.NotEmpty(t, found, "a dirty migration must be reported")
	require.Equal(t, fsck.SeverityError, found[0].Severity)
	require.Contains(t, found[0].Detail, "DIRTY")
}

// A prune that removes only some of its targets must report only what it
// actually deleted. Reporting the full candidate list alongside the error has
// the CLI print "pruned generated ref: X" for a ref still sitting in the store,
// and puts it in the JSON pruned_refs array.
func TestPruneGeneratedRefs_ReportsOnlyWhatItDeleted(t *testing.T) {
	svc := newVerifyRepo(t)

	head, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("agent/test"))
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("okf/main"), head.Hash())))

	// Closing the database makes every delete fail, so nothing is removed and
	// the result must name nothing.
	require.NoError(t, svc.rh.Storer().DB().Close())

	pruned, err := svc.PruneGeneratedRefs(context.Background())
	require.Error(t, err)
	require.Empty(t, pruned.Refs, "a prune that deleted nothing must claim nothing")
	require.Zero(t, pruned.Markers)
	require.True(t, pruned.Empty())
}

// Several checks iterate Go maps. Without an explicit sort two runs over an
// unchanged repo emit the same findings in different orders, and the reports
// cannot be diffed — which is most of what an operator does with them.
func TestVerify_ReportOrderIsStable(t *testing.T) {
	svc := newVerifyRepo(t)
	ctx := context.Background()

	// Enough distinct ghost rows that map iteration order would show through.
	var factID int64
	require.NoError(t, svc.rh.Storer().DB().QueryRow(`SELECT id FROM facts LIMIT 1`).Scan(&factID))
	head, err := svc.rh.HeadCommit(ctx, "agent/test")
	require.NoError(t, err)
	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md", "kb/e.md"} {
		_, err := svc.rh.Storer().DB().Exec(
			`INSERT INTO branch_facts(branch_id, path, fact_id, commit_hash)
			 VALUES ((SELECT id FROM branches WHERE name = 'agent/test'), ?, ?, ?)`,
			p, factID, head)
		require.NoError(t, err)
	}

	first, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)
	require.NotEmpty(t, first.Issues)
	for range 5 {
		next, err := svc.Verify(ctx, fsck.VerifyOpts{})
		require.NoError(t, err)
		require.Equal(t, first.Issues, next.Issues, "two runs over an unchanged repo must agree exactly")
	}
}

// Cancellation has to actually stop the walk. main.go wires SIGINT/SIGTERM into
// this context, and before this the signal could not interrupt a long verify.
func TestVerify_HonoursContextCancellation(t *testing.T) {
	svc := newVerifyRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.Verify(ctx, fsck.VerifyOpts{})
	require.ErrorIs(t, err, context.Canceled)
}
