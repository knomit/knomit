package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Round-6 tests: the upgrade runs once, at open, before release (user
// decision); every ref move derives first; rebuild's commit_log swap; one diff
// per commit.

func openPathHistoryStoreAt(t *testing.T) (*Service, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc, dbPath
}

func derivedCounts(t *testing.T, svc *Service) [3]int {
	t.Helper()
	var n [3]int
	for i, tbl := range []string{"path_changes", "path_change_links", "commit_fp"} {
		require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM `+tbl).Scan(&n[i]))
	}
	return n
}

func storedVersion(t *testing.T, svc *Service) string {
	t.Helper()
	v, err := metaGet(context.Background(), svc.rh.db, pathChangesVersionKey)
	require.NoError(t, err)
	return v
}

// TestOpenHistory_FailedPassKeepsRepoClosed replaces round 5's shadow pass
// (A3, and the B1/B2 races it had): the version change runs at open, in ONE
// transaction. A failure — a read error while preparing, or one after every
// row is applied — rolls it back and the repo does not open; nothing is left
// half-derived; the next open derives it.
func TestOpenHistory_FailedPassKeepsRepoClosed(t *testing.T) {
	for _, where := range []string{"prepare", "in the transaction"} {
		t.Run(where, func(t *testing.T) {
			svc, dbPath := openPathHistoryStoreAt(t)
			c := writeVersionsStore(t, svc, 4)
			want := historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[3]))
			staleVersion(t, svc)
			before := derivedCounts(t, svc)
			require.NoError(t, svc.Close())

			injected := errors.New("injected failure")
			if where == "prepare" {
				deriveObjectHook = func(kind string, h plumbing.Hash) error {
					if kind == "commit" && h.String() == c[1] {
						return injected
					}
					return nil
				}
			} else {
				openHistoryHook = func() error { return injected }
			}
			svc, err := Open(dbPath)
			require.NoError(t, err)
			err = svc.OpenRepo()
			deriveObjectHook, openHistoryHook = nil, nil
			require.ErrorIs(t, err, injected, "the repo does not open")
			require.Equal(t, before, derivedCounts(t, svc), "rolled back: nothing half-derived")
			require.Equal(t, "0", storedVersion(t, svc))
			require.NoError(t, svc.Close())

			svc, err = Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = svc.Close() })
			require.NoError(t, svc.OpenRepo(), "the next open retries")
			require.Equal(t, pathChangesVersion, storedVersion(t, svc))
			require.Equal(t, want, historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[3])))
		})
	}
}

// TestOpenHistory_UnresolvableBranchIsSkippedAndLogged covers B6 (A6): a
// branch whose ref fails to resolve does not block the open; it is logged and
// nothing is derived for it — its history is not served — while every other
// branch's is.
func TestOpenHistory_UnresolvableBranchIsSkippedAndLogged(t *testing.T) {
	svc, dbPath := openPathHistoryStoreAt(t)
	ctx := context.Background()
	c := writeVersionsStore(t, svc, 2)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "broken", "main"))
	b, err := svc.Facts().WriteFact(ctx, "broken", "kb/t.md", testFactBody("broken only", 0.5, nil), "b", "")
	require.NoError(t, err)
	clearDerived(t, svc, "") // an upgraded database
	require.NoError(t, svc.Close())

	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	branchTipHook = func(branch string) error {
		if branch == "broken" {
			return errors.New("injected ref read error")
		}
		return nil
	}
	svc, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	err = svc.OpenRepo()
	branchTipHook = nil
	require.NoError(t, err, "an unresolvable branch does not block the open")
	require.Contains(t, buf.String(), "branch ref unresolved")
	require.Contains(t, buf.String(), `"branch":"broken"`)
	require.Equal(t, pathChangesVersion, storedVersion(t, svc))
	require.Equal(t, reversed(c), historyCommits(fullHistory(t, svc, "main", "kb/t.md", c[1])))
	_, _, err = svc.Search().PathHistory(ctx, "broken", "kb/t.md", b.CommitHash, nil, 3)
	require.ErrorContains(t, err, "never derived", "nothing is served for the skipped branch")
}

// knownCommits returns every commit reachable from any ref.
func knownCommits(t *testing.T, svc *Service) map[plumbing.Hash]bool {
	t.Helper()
	known := map[plumbing.Hash]bool{}
	refs, err := svc.rh.repo.References()
	require.NoError(t, err)
	require.NoError(t, refs.ForEach(func(r *plumbing.Reference) error {
		if r.Type() != plumbing.HashReference {
			return nil
		}
		it, err := svc.rh.repo.Log(&gogit.LogOptions{From: r.Hash()})
		if err != nil {
			return nil
		}
		return it.ForEach(func(c *object.Commit) error { known[c.Hash] = true; return nil })
	}))
	return known
}

// failNewCommits makes every derivation read of a commit not in known fail,
// until the returned function is called.
func failNewCommits(known map[plumbing.Hash]bool, err error) func() {
	deriveObjectHook = func(kind string, h plumbing.Hash) error {
		if kind == "commit" && !known[h] {
			return err
		}
		return nil
	}
	return func() { deriveObjectHook = nil }
}

// TestRefMoves_DeriveBeforeTheRef covers B3 and B4: at the rebase
// fast-forward, the rebase replay's move, a rewind (whose repopulateBranch
// follows the move) and a write over a legacy branch_commits gap (whose
// AppendCommitLog populates), a derivation failure leaves the ref where it
// was — the history is derived BEFORE the ref moves.
func TestRefMoves_DeriveBeforeTheRef(t *testing.T) {
	injected := errors.New("injected read error")
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]func(t *testing.T, svc *Service) (branch string, run func() error){
		"rebase fast-forward": func(t *testing.T, svc *Service) (string, func() error) {
			agent := "agent/test"
			tip := mustHeadHash(t, svc, agent)
			u := writeRawCommit(t, svc, []string{tip.String()}, map[string]string{"kb/u.md": testFactBody("u", 0.5, nil)}, t0, "upstream")
			return agent, func() error {
				_, err := svc.rh.replayOntoUpstream(context.Background(), agent, plumbing.NewHash(u), plumbing.ZeroHash, StrategyLocalWins)
				return err
			}
		},
		"rebase replay": func(t *testing.T, svc *Service) (string, func() error) {
			agent := "agent/test"
			writeMergeFact(t, svc, agent, "kb/a.md", "A", "v1")
			base := mustHeadHash(t, svc, "main")
			u := writeRawCommit(t, svc, []string{base.String()}, map[string]string{"kb/u.md": testFactBody("u", 0.5, nil)}, t0, "upstream")
			return agent, func() error {
				_, err := svc.rh.replayOntoUpstream(context.Background(), agent, plumbing.NewHash(u), plumbing.ZeroHash, StrategyLocalWins)
				return err
			}
		},
		"rewind": func(t *testing.T, svc *Service) (string, func() error) {
			writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")
			root := writeRawCommit(t, svc, nil, map[string]string{"kb/r.md": testFactBody("r", 0.5, nil)}, t0, "disjoint")
			return "main", func() error {
				_, _, err := svc.rh.advanceBranchTo(context.Background(), "main", plumbing.NewHash(root))
				return err
			}
		},
		"write over a gap": func(t *testing.T, svc *Service) (string, func() error) {
			c := writeVersionsStore(t, svc, 3)
			_, err := svc.rh.db.Exec(`DELETE FROM branch_commits WHERE commit_hash = ? AND branch_id = (SELECT id FROM branches WHERE name = 'main')`, c[1])
			require.NoError(t, err)
			return "main", func() error {
				_, err := svc.Facts().WriteFact(context.Background(), "main", "kb/t.md", testFactBody("gap", 0.9, nil), "gap", "")
				return err
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = svc.Close() })
			require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
			branch, run := setup(t, svc)
			before := mustHeadHash(t, svc, branch)
			stop := failNewCommits(knownCommits(t, svc), injected)
			err = run()
			stop()
			require.ErrorIs(t, err, injected)
			require.Equal(t, before, mustHeadHash(t, svc, branch), "the ref did not move")
		})
	}
}

// branchCommitChanges counts inserts and deletes of branch's branch_commits
// rows from now on (a trigger, so it sees any implementation).
func branchCommitChanges(t *testing.T, svc *Service, branch string) func() int {
	t.Helper()
	var id int64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = ?`, branch).Scan(&id))
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS test_bc_changes (n INTEGER NOT NULL)`,
		`DELETE FROM test_bc_changes`,
		`INSERT INTO test_bc_changes VALUES (0)`,
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS test_bc_ins AFTER INSERT ON branch_commits WHEN NEW.branch_id = %d BEGIN UPDATE test_bc_changes SET n = n + 1; END`, id),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS test_bc_del AFTER DELETE ON branch_commits WHEN OLD.branch_id = %d BEGIN UPDATE test_bc_changes SET n = n + 1; END`, id),
	} {
		_, err := svc.rh.db.Exec(stmt)
		require.NoError(t, err)
	}
	return func() int {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(`SELECT n FROM test_bc_changes`).Scan(&n))
		return n
	}
}

// TestPathHistory_SwapIsProportional covers B5 (A4): a swap touches only the
// branch_commits rows that change — none for a rebuild of an unchanged
// branch, the dropped ones for a rewind — not the whole branch.
func TestPathHistory_SwapIsProportional(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c := writeVersionsStore(t, svc, 40)
	changes := branchCommitChanges(t, svc, "main")

	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Equal(t, 0, changes(), "a rebuild of an unchanged branch rewrites no branch_commits row")

	_, _, err := svc.rh.advanceBranchTo(ctx, "main", plumbing.NewHash(c[34]))
	require.NoError(t, err)
	require.Equal(t, 5, changes(), "a rewind by 5 commits deletes 5 rows")
}

// TestRebuild_ReplacesEveryCommitLogRowOfTheBranch covers B8: like dev's
// rebuild, every commit_log row of the branch's commits is rewritten from git
// — including bogus rows on a commit that changed nothing and on a dropped
// commit — while a dropped commit still on another branch keeps its rows.
func TestRebuild_ReplacesEveryCommitLogRowOfTheBranch(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c := writeVersionsStore(t, svc, 3)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "other", "main"))
	y, err := svc.Facts().WriteFact(ctx, "other", "kb/y.md", testFactBody("y", 0.5, nil), "y", "")
	require.NoError(t, err)
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	empty := rawCommit(t, svc, "main", []string{c[2]}, treeFiles(t, svc, c[2]), t0, "no change")
	x := writeRawCommit(t, svc, []string{c[2]}, map[string]string{"kb/x.md": "x"}, t0, "dropped")

	var mainID int64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT id FROM branches WHERE name = 'main'`).Scan(&mainID))
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO commit_log (commit_hash, path, message, action, committed_at) VALUES (?, 'kb/bogus.md', 'b', 'added', 0)`, []any{empty}},
		{`INSERT INTO commit_log (commit_hash, path, message, action, committed_at) VALUES (?, 'kb/bogus.md', 'b', 'added', 0)`, []any{c[2]}},
		{`INSERT INTO commit_log (commit_hash, path, message, action, committed_at) VALUES (?, 'kb/x.md', 'b', 'added', 0)`, []any{x}},
		{`INSERT INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`, []any{mainID, x}},
		{`INSERT INTO branch_commits (branch_id, commit_hash) VALUES (?, ?)`, []any{mainID, y.CommitHash}},
	} {
		_, err := svc.rh.db.Exec(stmt.q, stmt.args...)
		require.NoError(t, err)
	}
	yRows := commitLogRows(t, svc, y.CommitHash)
	require.NotEmpty(t, yRows)

	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Empty(t, commitLogRows(t, svc, empty), "a commit that changed nothing keeps no row")
	require.NotContains(t, commitLogRows(t, svc, c[2]), "kb/bogus.md added")
	require.Empty(t, commitLogRows(t, svc, x), "a dropped commit on no other branch loses its rows")
	require.Equal(t, yRows, commitLogRows(t, svc, y.CommitHash), "a dropped commit on another branch keeps them")
	for _, h := range c {
		co, err := svc.rh.repo.CommitObject(plumbing.NewHash(h))
		require.NoError(t, err)
		files, err := changedFilesInCommit(co)
		require.NoError(t, err)
		var want []string
		for _, f := range firstWins(files) {
			want = append(want, f.path+" "+f.action)
		}
		require.ElementsMatch(t, want, commitLogRows(t, svc, h), "%s is exactly its git diff", h)
	}
}

// TestRebuild_FailedSwapLeavesNothingStaged covers B9: a rebuild that fails
// between staging and its swap leaves no commit_log_stage rows, and rows a
// crash left behind are cleared by the next open.
func TestRebuild_FailedSwapLeavesNothingStaged(t *testing.T) {
	svc, dbPath := openPathHistoryStoreAt(t)
	ctx := context.Background()
	writeVersionsStore(t, svc, 3)
	stageRows := func() int {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM commit_log_stage`).Scan(&n))
		return n
	}
	injected := errors.New("injected swap failure")
	swapHook = func() error { return injected }
	err := svc.rh.rebuildCommitLog(ctx, "main")
	swapHook = nil
	require.ErrorIs(t, err, injected)
	require.Equal(t, 0, stageRows(), "a failed rebuild leaves nothing staged")

	_, err = svc.rh.db.Exec(`INSERT INTO commit_log_stage (branch_id, commit_hash, path, message, committed_at) VALUES (1, 'dead', 'kb/x.md', '', 0)`)
	require.NoError(t, err)
	svc = reopen(t, svc, dbPath)
	require.Equal(t, 0, stageRows(), "open clears a crashed rebuild's stage")
}

// TestIndex_OneDiffPerCommit covers B7: a write, and a populate, diff each
// commit's trees once — commit_log's payload is derivation's own change list.
func TestIndex_OneDiffPerCommit(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	writeVersionsStore(t, svc, 2)
	n := commitDiffs.Load()
	_, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("one diff", 0.9, nil), "w", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), commitDiffs.Load()-n, "a write diffs its commit once")

	tip := mustHeadHash(t, svc, "main")
	prev := []string{tip.String()}
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		prev = []string{writeRawCommit(t, svc, prev, map[string]string{"kb/t.md": testFactBody(fmt.Sprintf("p%d", i), 0.5, nil)}, t0.Add(time.Duration(i)*time.Minute), "p")}
	}
	moveBranch(t, svc, "main", prev[0])
	n = commitDiffs.Load()
	require.NoError(t, svc.rh.populateCommitLog(ctx, "main"))
	require.Equal(t, int64(5), commitDiffs.Load()-n, "a populate diffs each commit once")
}

// TestCommitChanges_EqualsGitDiff pins derivation's tree diff to go-git's
// DiffTree (commit_log's former payload) under commit_log's first-row-wins
// rule, on the shapes that differ between tree walks: nested adds and deletes,
// a file replaced by a directory of the same name, a case-only rename, a
// commit that changes nothing, and a merge (diffed against parent 0).
func TestCommitChanges_EqualsGitDiff(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	body := func(s string) string { return testFactBody(s, 0.5, nil) }
	var commits []string
	step := func(parents []string, files map[string]string) string {
		h := writeRawCommit(t, svc, parents, files, t0.Add(time.Duration(len(commits))*time.Minute), "s")
		commits = append(commits, h)
		return h
	}
	a := step(nil, map[string]string{"kb/a/x.md": body("x"), "kb/a/y/z.md": body("z"), "kb/b": "file", "kb/b.md": body("b")})
	b := step([]string{a}, map[string]string{"kb/a/x.md": body("x2"), "kb/b/c.md": body("c"), "kb/b.md": body("b"), "kb/n/deep/e.md": body("e")})
	c := step([]string{b}, map[string]string{"kb/A/x.md": body("x2"), "kb/b/c.md": body("c"), "kb/b.md": body("b"), "kb/n/deep/e.md": body("e")})
	d := step([]string{c}, map[string]string{"kb/A/x.md": body("x2"), "kb/b/c.md": body("c"), "kb/b.md": body("b"), "kb/n/deep/e.md": body("e")})
	side := step([]string{a}, map[string]string{"kb/a/x.md": body("side"), "kb/s.md": body("s")})
	step([]string{d, side}, map[string]string{"kb/A/x.md": body("merged"), "kb/s.md": body("s"), "kb/b.md": body("b")})

	for _, h := range commits {
		co, err := svc.rh.repo.CommitObject(plumbing.NewHash(h))
		require.NoError(t, err)
		files, err := changedFilesInCommit(co)
		require.NoError(t, err)
		got, err := newDeriver(svc.rh, activeTables).commitChanges(co)
		require.NoError(t, err)
		gotFiles := make([]changedFileEntry, len(got))
		for i, e := range got {
			gotFiles[i] = changedFileEntry{path: e.Path, action: e.Action}
		}
		require.Equal(t, firstWins(files), firstWins(gotFiles), "commit %s", h)
	}
}

// firstWins keeps the first entry per path — what commit_log stores (INSERT OR
// IGNORE on (commit, path)) — sorted by path.
func firstWins(files []changedFileEntry) []changedFileEntry {
	seen := map[string]bool{}
	var out []changedFileEntry
	for _, f := range files {
		if !seen[f.path] {
			seen[f.path] = true
			out = append(out, f)
		}
	}
	sortEntries(out)
	return out
}

func sortEntries(es []changedFileEntry) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].path < es[j-1].path; j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

func commitLogRows(t *testing.T, svc *Service, commit string) []string {
	t.Helper()
	rows, err := svc.rh.db.Query(`SELECT path, action FROM commit_log WHERE commit_hash = ? ORDER BY path`, commit)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p, a string
		require.NoError(t, rows.Scan(&p, &a))
		out = append(out, p+" "+a)
	}
	return out
}

// treeFiles returns every file in commit's tree, path case preserved.
func treeFiles(t *testing.T, svc *Service, commit string) map[string]string {
	t.Helper()
	co, err := svc.rh.repo.CommitObject(plumbing.NewHash(commit))
	require.NoError(t, err)
	tree, err := co.Tree()
	require.NoError(t, err)
	out := map[string]string{}
	require.NoError(t, tree.Files().ForEach(func(f *object.File) error {
		s, err := f.Contents()
		if err != nil {
			return err
		}
		out[f.Name] = s
		return nil
	}))
	return out
}

// TestOpenHistory_KeepsNoChangeLists covers C1: the open-time pass never
// indexes what it derives, so it must not park their tree diffs in
// rh.changes (they would pile up per repo, and dropping them wholesale at the
// cap would cost a concurrent write its one diff).
func TestOpenHistory_KeepsNoChangeLists(t *testing.T) {
	svc, dbPath := openPathHistoryStoreAt(t)
	writeVersionsStore(t, svc, 5)
	clearDerived(t, svc, "") // an upgraded database
	svc = reopen(t, svc, dbPath)
	svc.rh.changes.mu.Lock()
	defer svc.rh.changes.mu.Unlock()
	require.Empty(t, svc.rh.changes.m, "the open-time pass keeps no change lists")
	require.Zero(t, svc.rh.changes.n)
}

// TestRebuild_UnchangedBranchWritesNoCommitLogRow covers C3: a rebuild of a
// branch whose commit_log already matches git rewrites no commit_log row in
// its swap, and leaves nothing staged — while a row that differs is still
// replaced (B8).
func TestRebuild_UnchangedBranchWritesNoCommitLogRow(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c := writeVersionsStore(t, svc, 5)
	for _, stmt := range []string{
		`CREATE TABLE test_cl_writes (n INTEGER NOT NULL)`,
		`INSERT INTO test_cl_writes VALUES (0)`,
		`CREATE TRIGGER test_cl_ins AFTER INSERT ON commit_log BEGIN UPDATE test_cl_writes SET n = n + 1; END`,
		`CREATE TRIGGER test_cl_del AFTER DELETE ON commit_log BEGIN UPDATE test_cl_writes SET n = n + 1; END`,
	} {
		_, err := svc.rh.db.Exec(stmt)
		require.NoError(t, err)
	}
	writes := func() int {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(`SELECT n FROM test_cl_writes`).Scan(&n))
		_, err := svc.rh.db.Exec(`UPDATE test_cl_writes SET n = 0`)
		require.NoError(t, err)
		return n
	}
	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Equal(t, 0, writes(), "an unchanged rebuild writes no commit_log row")
	var staged int
	require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM commit_log_stage`).Scan(&staged))
	require.Zero(t, staged, "nothing is left staged")

	_, err := svc.rh.db.Exec(`UPDATE commit_log SET message = 'tampered' WHERE commit_hash = ?`, c[2])
	require.NoError(t, err)
	writes()
	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Equal(t, 2*len(commitLogRows(t, svc, c[2])), writes(), "only the differing rows are replaced")
	var msg string
	require.NoError(t, svc.rh.db.QueryRow(`SELECT message FROM commit_log WHERE commit_hash = ? LIMIT 1`, c[2]).Scan(&msg))
	require.NotEqual(t, "tampered", msg)
}
