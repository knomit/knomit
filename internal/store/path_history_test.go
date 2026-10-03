package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

func openPathHistoryStore(t *testing.T) (*Service, context.Context) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	return svc, context.Background()
}

// fullHistory pages PathHistory to the end with a small page size, so every
// test also exercises the keyset continuation.
func fullHistory(t *testing.T, svc *Service, branch, path, anchor string) []FactRevision {
	t.Helper()
	ctx := context.Background()
	var out []FactRevision
	var cur *PathHistoryCursor
	for range 1000 {
		page, next, err := svc.Search().PathHistory(ctx, branch, path, anchor, cur, 2)
		require.NoError(t, err)
		out = append(out, page...)
		if next == nil {
			return out
		}
		require.NotEmpty(t, page, "a continuation position must come with a non-empty page")
		cur = next
	}
	t.Fatal("history paging did not terminate")
	return nil
}

func historyCommits(revs []FactRevision) []string {
	out := make([]string, len(revs))
	for i, r := range revs {
		out[i] = r.Commit
	}
	return out
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

func blobOf(t *testing.T, svc *Service, commit, path string) string {
	t.Helper()
	co, err := svc.rh.repo.CommitObject(plumbing.NewHash(commit))
	require.NoError(t, err)
	tree, err := co.Tree()
	require.NoError(t, err)
	e, err := treeEntryInsensitive(svc.rh.repo, tree, path)
	require.NoError(t, err)
	return e.Hash.String()
}

// rawCommit writes a commit whose tree is exactly files (path -> content,
// path case preserved) with the given parents and author date, moves branch
// to it and indexes it. For topologies and dates the write API cannot make.
func rawCommit(t *testing.T, svc *Service, branch string, parents []string, files map[string]string, author time.Time, msg string) string {
	t.Helper()
	h := writeRawCommit(t, svc, parents, files, author, msg)
	moveBranch(t, svc, branch, h)
	require.NoError(t, svc.rh.populateCommitLog(context.Background(), branch))
	return h
}

// moveBranch points branch at h (registering it) WITHOUT indexing anything.
func moveBranch(t testing.TB, svc *Service, branch, h string) {
	t.Helper()
	_, err := svc.rh.EnsureBranch(context.Background(), branch, "refs/heads/"+branch)
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), plumbing.NewHash(h))))
}

// writeRawCommit writes a commit object whose tree is exactly files (path
// case preserved) and returns its hash. It indexes nothing and moves no ref.
func writeRawCommit(t testing.TB, svc *Service, parents []string, files map[string]string, author time.Time, msg string) string {
	t.Helper()
	st := svc.rh.repo.Storer
	type dir struct {
		files map[string]plumbing.Hash
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{files: map[string]plumbing.Hash{}, dirs: map[string]*dir{}} }
	root := newDir()
	for p, content := range files {
		obj := st.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		h, err := st.SetEncodedObject(obj)
		require.NoError(t, err)
		parts := strings.Split(p, "/")
		d := root
		for _, seg := range parts[:len(parts)-1] {
			if d.dirs[seg] == nil {
				d.dirs[seg] = newDir()
			}
			d = d.dirs[seg]
		}
		d.files[parts[len(parts)-1]] = h
	}
	var writeTree func(d *dir) plumbing.Hash
	writeTree = func(d *dir) plumbing.Hash {
		var entries []object.TreeEntry
		for name, h := range d.files {
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
		}
		for name, sub := range d.dirs {
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: writeTree(sub)})
		}
		sortKey := func(e object.TreeEntry) string {
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		sort.Slice(entries, func(i, j int) bool { return sortKey(entries[i]) < sortKey(entries[j]) })
		obj := st.NewEncodedObject()
		require.NoError(t, (&object.Tree{Entries: entries}).Encode(obj))
		h, err := st.SetEncodedObject(obj)
		require.NoError(t, err)
		return h
	}
	sig := object.Signature{Name: "test", Email: "test@local", When: author}
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: writeTree(root)}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, plumbing.NewHash(p))
	}
	obj := st.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := st.SetEncodedObject(obj)
	require.NoError(t, err)
	return h.String()
}

// TestPathHistory_MergeDeliveredWrites is the regression for the explain
// history walk that listed one PR merge per PR and hid every write behind it.
// Every write lands on `agent`, reaches main through a real two-parent merge,
// and `agent` then catches up with main. The history must be exactly the
// write commits, newest first, on both branches, with no merge commit in it.
func TestPathHistory_MergeDeliveredWrites(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"

	created, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("v0", 0.9, nil), "create", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))

	writes := []string{created.CommitHash}
	var merges []string
	for k := range 5 {
		for j := range 3 {
			r, err := svc.Facts().WriteFact(ctx, "agent", p,
				testFactBody(fmt.Sprintf("pr%d w%d", k, j), 0.5, nil), fmt.Sprintf("pr%d w%d", k, j), "")
			require.NoError(t, err)
			writes = append(writes, r.CommitHash)
		}
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/other%d.md", k), testFactBody("x", 0.5, nil), "other", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
		tip, err := svc.Branches().HeadCommit(ctx, "main")
		require.NoError(t, err)
		mc, err := svc.rh.repo.CommitObject(plumbing.NewHash(tip))
		require.NoError(t, err)
		require.Len(t, mc.ParentHashes, 2, "setup must produce a two-parent PR merge")
		merges = append(merges, tip)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "main", "agent", StrategyLocalWins))
	}

	fp, err := svc.Search().RevisionsBefore(ctx, "main", p, merges[len(merges)-1], 100)
	require.NoError(t, err)
	require.Len(t, fp, len(merges)+1, "precondition: first-parent sees one revision per PR plus the creation")

	want := reversed(writes)
	for _, br := range []string{"main", "agent"} {
		anchor, err := svc.Branches().HeadCommit(ctx, br)
		require.NoError(t, err)
		revs := fullHistory(t, svc, br, p, anchor)
		require.Equal(t, want, historyCommits(revs), "%s: every write, newest first, and nothing else", br)
		require.Equal(t, "added", revs[len(revs)-1].Action, "%s: the oldest entry is the creation", br)
		require.Empty(t, revs[len(revs)-1].FromBlob)
		for i, r := range revs[:len(revs)-1] {
			require.Equal(t, "modified", r.Action)
			require.NotZero(t, r.AuthoredAt)
			require.Equal(t, revs[i+1].Blob, r.FromBlob, "on a chain each change is edited from the one before it")
		}
	}
}

// TestPathHistory_ReplayedCommitStaysAboveItsAncestors is the regression for
// review finding 1: a rebase replay keeps the ORIGINAL author date, so a
// replayed change can carry a date older than the change it now sits on. The
// history must still list it first — it is the content live at the anchor.
func TestPathHistory_ReplayedCommitStaysAboveItsAncestors(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	const p = "kb/t.md"
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	base := rawCommit(t, svc, "main", nil, map[string]string{p: testFactBody("base", 0.5, nil)}, t0, "base")
	onMain := rawCommit(t, svc, "main", []string{base}, map[string]string{p: testFactBody("main edit", 0.6, nil)}, t0.Add(48*time.Hour), "main edit")
	// The replayed agent write: authored a day BEFORE the main edit it now follows.
	replayed := rawCommit(t, svc, "main", []string{onMain}, map[string]string{p: testFactBody("agent edit", 0.7, nil)}, t0.Add(24*time.Hour), "agent edit (replayed)")

	revs := fullHistory(t, svc, "main", p, replayed)
	require.Equal(t, []string{replayed, onMain, base}, historyCommits(revs))
	require.Equal(t, t0.Add(24*time.Hour).Unix(), revs[0].AuthoredAt, "the displayed date is the change's own author date")
}

// TestPathHistory_SkewedDateNeverBelowItsAncestor: the key matters once the
// walk holds two changes at once. X is edited on main (C1, later) and on a
// side (C2, a replayed change dated BEFORE X), and a merge composes both. By
// raw author date X would pop before C2 — an ancestor above its descendant.
// The precomputed key lifts C2 to X's date, and its generation puts it first.
func TestPathHistory_SkewedDateNeverBelowItsAncestor(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	const p = "kb/t.md"
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

	x := rawCommit(t, svc, "main", nil, map[string]string{p: testFactBody("X", 0.5, nil)}, at(10), "X")
	c2 := rawCommit(t, svc, "side", []string{x}, map[string]string{p: testFactBody("X", 0.7, nil)}, at(5), "C2 (replayed, old date)")
	c1 := rawCommit(t, svc, "main", []string{x}, map[string]string{p: testFactBody("X body", 0.5, nil)}, at(20), "C1")
	z := rawCommit(t, svc, "main", []string{c1, c2}, map[string]string{p: testFactBody("X body", 0.7, nil)}, at(30), "Z composes")

	revs := fullHistory(t, svc, "main", p, z)
	require.Equal(t, []string{z, c1, c2, x}, historyCommits(revs), "C2 descends from X, so it is never listed below X")
}

// TestPathHistory_DiffBaseIsTheEditedFromContent is the regression for review
// finding 2: each change's FromBlob is the content it was edited from, never
// its neighbour in the list. Fork at B; X (side) sets confidence 0.7; Y (main)
// edits the body; a merge composes both. Y must be based on B, not on X.
func TestPathHistory_DiffBaseIsTheEditedFromContent(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	const p = "kb/t.md"

	b, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("B", 0.5, nil), "B", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "side", "main"))
	x, err := svc.Facts().WriteFact(ctx, "side", p, testFactBody("B", 0.7, nil), "X: confidence", "")
	require.NoError(t, err)
	y, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody("B edited", 0.5, nil), "Y: body", "")
	require.NoError(t, err)
	_, err = svc.rh.mergeIntoBranchResolved(ctx, "side", "main", StrategyRefuse, map[string]Resolution{
		p: {Body: []byte(testFactBody("B edited", 0.7, nil))},
	})
	require.NoError(t, err)
	m, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	revs := fullHistory(t, svc, "main", p, m)
	byCommit := map[string]FactRevision{}
	for _, r := range revs {
		byCommit[r.Commit] = r
	}
	require.Len(t, byCommit, 4)
	require.Equal(t, m, revs[0].Commit, "the composing merge is the live content")
	bBlob := blobOf(t, svc, b.CommitHash, p)
	require.Equal(t, bBlob, byCommit[y.CommitHash].FromBlob, "Y was edited from B")
	require.Equal(t, bBlob, byCommit[x.CommitHash].FromBlob, "X was edited from B")
	require.Equal(t, blobOf(t, svc, y.CommitHash, p), byCommit[m].FromBlob, "a composing merge diffs against its first parent")
	// The STORED diff agrees with FromBlob: against Y (first parent) the merge
	// changed only confidence 0.5 -> 0.7; against X (second parent) it would
	// have been a body change instead.
	require.NotNil(t, byCommit[m].Diff)
	require.Equal(t, []float64{0.5, 0.7}, byCommit[m].Diff.Confidence)
	require.Empty(t, byCommit[m].Diff.Body)
	require.Equal(t, []float64{0.5, 0.7}, byCommit[x.CommitHash].Diff.Confidence)
	// Y changed only the title (untracked) against B, so it has no diff;
	// against its list neighbour X it would have shown a fake 0.7 -> 0.5.
	require.Nil(t, byCommit[y.CommitHash].Diff)
	require.Equal(t, b.CommitHash, revs[len(revs)-1].Commit)
}

// TestPathHistory_MixedCasePath is the regression for review finding 3: a
// fact stored under a mixed-case path (kb/Topic/X.md, from a repo written by
// other tools) is looked up case-insensitively, so a PR merge is recognised
// as carrying the side's content and the history keeps the write behind it.
func TestPathHistory_MixedCasePath(t *testing.T) {
	svc, _ := openPathHistoryStore(t)
	const real = "kb/Topic/X.md"
	const p = "kb/topic/x.md"
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	base := rawCommit(t, svc, "main", nil, map[string]string{real: testFactBody("v1", 0.5, nil)}, t0, "v1")
	side := rawCommit(t, svc, "side", []string{base}, map[string]string{real: testFactBody("v2", 0.6, nil)}, t0.Add(time.Hour), "v2 on side")
	other := rawCommit(t, svc, "main", []string{base}, map[string]string{real: testFactBody("v1", 0.5, nil), "kb/Other.md": "x"}, t0.Add(2*time.Hour), "other on main")
	merge := rawCommit(t, svc, "main", []string{other, side}, map[string]string{real: testFactBody("v2", 0.6, nil), "kb/Other.md": "x"}, t0.Add(3*time.Hour), "Merge pull request")

	revs := fullHistory(t, svc, "main", p, merge)
	require.Equal(t, []string{side, base}, historyCommits(revs), "the PR merge carries the side's write; it is not a change")
}

// TestPathHistory_LocalWinsLoserExcluded: a feature write that a LocalWins
// merge discarded never reached main's content, so it is not in main's
// history although its commit is reachable through the merge's second parent.
func TestPathHistory_LocalWinsLoserExcluded(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)

	v1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "v1 on main", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	loser, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "v2 on feature", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "feature", "kb/other.md", testFactBody("side", 0.5, nil), "side", "")
	require.NoError(t, err)
	v3, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v3", 0.7, nil), "v3 on main", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().MergeBranch(ctx, "feature", "main", StrategyLocalWins))

	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	revs := fullHistory(t, svc, "main", "kb/t.md", tip)
	require.Equal(t, []string{v3.CommitHash, v1.CommitHash}, historyCommits(revs))
	require.NotContains(t, historyCommits(revs), loser.CommitHash)
}

// TestPathHistory_RevertIsAChange: returning to an earlier blob is a new
// change — the newest entry must be the content live at the anchor.
func TestPathHistory_RevertIsAChange(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	a := testFactBody("A", 0.9, nil)
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", a, "A", "")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("B", 0.9, nil), "B", "")
	require.NoError(t, err)
	r3, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", a, "back to A", "")
	require.NoError(t, err)

	revs := fullHistory(t, svc, "main", "kb/t.md", r3.CommitHash)
	require.Equal(t, []string{r3.CommitHash, r2.CommitHash, r1.CommitHash}, historyCommits(revs))
	// Anchored before the revert, the live A is the ORIGINAL write.
	revs = fullHistory(t, svc, "main", "kb/t.md", r2.CommitHash)
	require.Equal(t, []string{r2.CommitHash, r1.CommitHash}, historyCommits(revs))
}

// TestPathHistory_LinearMatchesFirstParent: on a linear history the change
// list is exactly the first-parent list, bounded to the anchor.
func TestPathHistory_LinearMatchesFirstParent(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	c1, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs := fullHistory(t, svc, "main", "kb/t.md", c3)
	require.Equal(t, []string{c3, c2, c1}, historyCommits(revs))
	require.Equal(t, "edit t again", revs[0].Message)
	require.Equal(t, "added", revs[2].Action)

	revs = fullHistory(t, svc, "main", "kb/t.md", c2)
	require.Equal(t, []string{c2, c1}, historyCommits(revs))
}

// TestPathHistory_ScopedToBranch: an anchor committed on another branch
// contributes nothing off-branch; its on-branch ancestry still counts.
func TestPathHistory_ScopedToBranch(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody("v1", 0.9, nil), "create t", "")
	require.NoError(t, err)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	r2, err := svc.Facts().WriteFact(ctx, "feature", "kb/t.md", testFactBody("v2", 0.8, nil), "edit on feature", "")
	require.NoError(t, err)

	require.Equal(t, []string{r1.CommitHash}, historyCommits(fullHistory(t, svc, "main", "kb/t.md", r2.CommitHash)))
	require.Equal(t, []string{r2.CommitHash, r1.CommitHash}, historyCommits(fullHistory(t, svc, "feature", "kb/t.md", r2.CommitHash)))
}

// TestPathHistory_ContinuationAfterPurgeIsHistoryChanged is the store half of
// review finding 4: once the branch's commit visibility is purged (a
// reconcileMain rewind, or a commit_log rebuild in progress), a continuation
// must fail with ErrHistoryChanged — never return a short or empty page.
func TestPathHistory_ContinuationAfterPurgeIsHistoryChanged(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	var last string
	for i := range 5 {
		r, err := svc.Facts().WriteFact(ctx, "main", "kb/t.md", testFactBody(fmt.Sprintf("v%d", i), 0.5, nil), "v", "")
		require.NoError(t, err)
		last = r.CommitHash
	}
	_, cur, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", last, nil, 2)
	require.NoError(t, err)
	require.NotNil(t, cur)

	// A purge of the branch's visibility (what a rewind in progress looked
	// like before the swap became one transaction).
	_, err = svc.rh.db.ExecContext(ctx, `DELETE FROM branch_commits WHERE branch_id = (SELECT id FROM branches WHERE name = 'main')`)
	require.NoError(t, err)
	_, _, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", last, cur, 2)
	require.ErrorIs(t, err, ErrHistoryChanged)

	// Healed (repopulated): the same position continues correctly.
	require.NoError(t, svc.rh.populateCommitLog(ctx, "main"))
	page, _, err := svc.Search().PathHistory(ctx, "main", "kb/t.md", last, cur, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)

	// A position naming a change that is not indexed is refused, not empty.
	_, _, err = svc.Search().PathHistory(ctx, "main", "kb/t.md", last, &PathHistoryCursor{Frontier: []string{strings.Repeat("ab", 20)}, AnchorOnBranch: true}, 2)
	require.ErrorIs(t, err, ErrHistoryChanged)
}

// TestPathHistory_RebuildAndVersionResetAreConsistent: the derived rows are
// content-addressed — a commit_log rebuild leaves the history unchanged, and a
// derivation-version reset recomputes exactly the same history.
func TestPathHistory_RebuildAndVersionResetAreConsistent(t *testing.T) {
	svc, ctx := openPathHistoryStore(t)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent", "main"))
	for k := range 3 {
		for j := range 2 {
			_, err := svc.Facts().WriteFact(ctx, "agent", "kb/t.md", testFactBody(fmt.Sprintf("%d-%d", k, j), 0.5, nil), "w", "")
			require.NoError(t, err)
		}
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/o%d.md", k), testFactBody("o", 0.5, nil), "o", "")
		require.NoError(t, err)
		require.NoError(t, svc.Branches().MergeBranch(ctx, "agent", "main", StrategyLocalWins))
	}
	tip, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	before := historyCommits(fullHistory(t, svc, "main", "kb/t.md", tip))
	require.Len(t, before, 6)

	require.NoError(t, svc.rh.rebuildCommitLog(ctx, "main"))
	require.Equal(t, before, historyCommits(fullHistory(t, svc, "main", "kb/t.md", tip)))

	_, err = svc.rh.db.ExecContext(ctx, `UPDATE meta SET value = 'stale' WHERE key = ?`, pathChangesVersionKey)
	require.NoError(t, err)
	require.NoError(t, svc.rh.openHistory(ctx))
	require.Equal(t, before, historyCommits(fullHistory(t, svc, "main", "kb/t.md", tip)))
}
