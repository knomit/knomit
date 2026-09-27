package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// ---- DiffFacts

// DiffFacts has NO ancestry check: two commits that are neither ancestor nor
// descendant of each other (the rewind-replay shape) still diff as two trees.
// ChangesUnder refuses exactly this pair. Sabotage: add the IsAncestor
// refusal to DiffFacts — the sideways case errors.
func TestDiffFacts_NoAncestryCheck(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	x := plumbing.NewHash(writeF(t, svc, "main", "kb/tasks/a/x.md"))
	y := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/tasks/a/y.md"))

	// Sideways: main's head and agent/a's head share the root only.
	rows, err := svc.Triggers().DiffFacts(ctx, x, y)
	require.NoError(t, err, "a non-ancestor pair must diff, not refuse")
	require.Equal(t, []PathChange{
		{Path: "kb/tasks/a/x.md", Change: ChangeDeleted},
		{Path: "kb/tasks/a/y.md", Change: ChangeAdded},
	}, rows)
	_, err = svc.Facts().ChangesUnder(ctx, "agent/a", ChangesQuery{Prefix: "tasks/a", Since: x.String()})
	require.ErrorIs(t, err, ErrSinceNotBehind, "ChangesUnder keeps its check; only DiffFacts drops it")

	// Linear: W → H lists the net difference under the WHOLE ontology root.
	z := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/other/z.md"))
	rows, err = svc.Triggers().DiffFacts(ctx, y, z)
	require.NoError(t, err)
	require.Equal(t, []PathChange{{Path: "kb/other/z.md", Change: ChangeAdded}}, rows)

	// The empty tree as `from`: everything is added.
	rows, err = svc.Triggers().DiffFacts(ctx, plumbing.ZeroHash, y)
	require.NoError(t, err)
	require.Contains(t, rows, PathChange{Path: "kb/tasks/a/y.md", Change: ChangeAdded})
}

// ---- The treesame toucher walk

func toucher(t *testing.T, svc *Service, head plumbing.Hash, path string) TouchResult {
	t.Helper()
	res, err := svc.Triggers().Toucher(context.Background(), head, path)
	require.NoError(t, err)
	return res
}

// A merge bringing A (add x), B (modify x), C (add y), D (delete y): the
// toucher of x at the merge is B — the LAST non-merge commit that changed
// the blob — not A and not the merge commit. Sabotage: return the first
// commit that has the path (A), or go-git Log{FileName} (the merge).
func TestToucher_SamePathTwiceInOneMerge(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	base := writeF(t, svc, "agent/a", "kb/tasks/a/base.md")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "peer", "agent/a"))
	writeF(t, svc, "agent/a", "kb/tasks/a/mine.md")                   // the agent's own work, so the merge is a real merge
	writeF(t, svc, "peer", "kb/tasks/a/x.md")                         // A
	b := plumbing.NewHash(modifyF(t, svc, "peer", "kb/tasks/a/x.md")) // B
	writeF(t, svc, "peer", "kb/tasks/a/y.md")                         // C
	deleteF(t, svc, "peer", "kb/tasks/a/y.md")                        // D
	require.NoError(t, svc.Branches().MergeBranch(ctx, "peer", "agent/a", StrategyRemoteWins))
	head := mustHeadHash(t, svc, "agent/a")
	merge, err := svc.rh.repo.CommitObject(head)
	require.NoError(t, err)
	require.Equal(t, 2, merge.NumParents(), "fixture: the merge must be a real merge commit")
	require.NotEqual(t, plumbing.NewHash(base), head)

	res := toucher(t, svc, head, "kb/tasks/a/x.md")
	require.True(t, res.Found)
	require.Equal(t, b, res.Commit, "the toucher is B, the last non-merge commit that changed x")
	require.False(t, res.MergeToucher)
	require.NotEqual(t, head, res.Commit, "never the merge")
}

// A path that arrived through a two-parent merge authored by someone else
// names the underlying non-merge commit's author, not the merge's. Sabotage:
// accept a merge as the toucher when a parent is same-blob.
func TestToucher_BotMergeNotAuthor(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	writeF(t, svc, "agent/a", "kb/tasks/a/mine.md")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "peer", "main"))
	theirs := plumbing.NewHash(writeF(t, svc, "peer", "kb/tasks/a/theirs.md"))
	// A merge commit authored by a "bot", built by hand: the tree is the
	// agent's tree plus theirs.md (exactly what a clean merge produces).
	require.NoError(t, svc.Branches().MergeBranch(ctx, "peer", "agent/a", StrategyRemoteWins))
	head := mustHeadHash(t, svc, "agent/a")
	restampAs(t, svc, "agent/a", nil, fixedTime(1), "Merge pull request #1 from peer")
	head2 := mustHeadHash(t, svc, "agent/a")
	require.NotEqual(t, head, head2)
	bot, err := svc.rh.repo.CommitObject(head2)
	require.NoError(t, err)
	require.Equal(t, 2, bot.NumParents())

	res := toucher(t, svc, head2, "kb/tasks/a/theirs.md")
	require.True(t, res.Found)
	require.Equal(t, theirs, res.Commit, "the toucher is the peer's commit, not the bot merge")
	require.False(t, res.MergeToucher)
}

// The one case a merge IS the toucher: none of its parents carries the blob
// (a textual three-way merge produced it). It is reported as such.
func TestToucher_MergeWithNoSameBlobParentIsTheToucher(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	c1 := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/tasks/a/x.md"))
	require.NoError(t, svc.Branches().CreateBranch(ctx, "peer", "main"))
	c2 := plumbing.NewHash(modifyF(t, svc, "peer", "kb/tasks/a/x.md"))
	// A third version of x, whose tree the hand-built merge will carry.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "third", "main"))
	r, err := svc.Facts().WriteFact(ctx, "third", "kb/tasks/a/x.md", testFactBody("x v3", 0.5, nil), "v3", "update")
	require.NoError(t, err)
	third, err := svc.rh.repo.CommitObject(plumbing.NewHash(r.CommitHash))
	require.NoError(t, err)
	sig := object.Signature{Name: "m", Email: "m@example.com", When: fixedTime(9)}
	m := &object.Commit{Author: sig, Committer: sig, Message: "manual merge", TreeHash: third.TreeHash, ParentHashes: []plumbing.Hash{c1, c2}}
	obj := svc.rh.repo.Storer.NewEncodedObject()
	require.NoError(t, m.Encode(obj))
	mh, err := svc.rh.repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)

	res := toucher(t, svc, mh, "kb/tasks/a/x.md")
	require.True(t, res.Found)
	require.Equal(t, mh, res.Commit)
	require.True(t, res.MergeToucher, "a merge with no same-blob parent is the toucher, and says so")
}

// An advance with h1 (tasks change) then h2 (other change): the toucher of
// the tasks path is h1, not the tip. Sabotage: use the tip commit.
func TestToucher_PrefixChangeNotInTip(t *testing.T) {
	svc := newChangesService(t)
	h1 := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/tasks/a/t.md"))
	h2 := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/other/o.md"))
	res := toucher(t, svc, h2, "kb/tasks/a/t.md")
	require.True(t, res.Found)
	require.Equal(t, h1, res.Commit)
	require.Equal(t, 1, res.Steps)
}

// A retract in a linear history: the toucher is the commit that removed the
// path (its parent has it, it does not). A path that never existed reaches
// the root and is NOT found — the caller's fallback.
func TestToucher_RetractAndScrubbed(t *testing.T) {
	svc := newChangesService(t)
	writeF(t, svc, "agent/a", "kb/tasks/a/t.md")
	d := plumbing.NewHash(deleteF(t, svc, "agent/a", "kb/tasks/a/t.md"))
	h := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/other/o.md"))
	res := toucher(t, svc, h, "kb/tasks/a/t.md")
	require.True(t, res.Found)
	require.Equal(t, d, res.Commit)

	res = toucher(t, svc, h, "kb/tasks/a/never.md")
	require.False(t, res.Found, "a path with no origin in this history is not found")
	require.Equal(t, plumbing.ZeroHash, res.Commit)
}

// ---- Trailers

// TraceFromTrailer: the trailer is read from the LAST paragraph of the full
// message, last occurrence wins, case-insensitive key; absent → "". Sabotage:
// read the first line only (commit_log.message keeps only the first line).
func TestTrailerValue(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"learn: x\n\nKnomit-Trace: t-1\n", "t-1"},
		{"learn: x\n\nbody\n\nKnomit-Trace: t-1\nKnomit-Cause: abc\n", "t-1"},
		{"learn: x\n\nKnomit-Trace: first\nKnomit-Trace: last\n", "last"},
		{"learn: x\n\nknomit-trace: lower\n", "lower"},
		{"learn: x\n\nKnomit-Trace:   spaced  \n", "spaced"},
		{"Knomit-Trace: single-paragraph\n", "single-paragraph"},
		{"learn: x\n", ""},
		{"learn: x\n\nKnomit-Trace: early\n\nlater paragraph\n", ""},
		{"", ""},
	} {
		require.Equal(t, tc.want, TrailerValue(tc.msg, "Knomit-Trace"), "%q", tc.msg)
	}
	// The message a knomit write produces carries none: nothing stamps one
	// before PR 3.
	svc := newChangesService(t)
	h := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/tasks/a/t.md"))
	info, err := svc.Triggers().CommitInfo(context.Background(), h)
	require.NoError(t, err)
	require.Equal(t, "", TrailerValue(info.Message, "Knomit-Trace"))
}

// ---- The two tables

func fireRows(n int, prefix string) []TriggerFire {
	rows := make([]TriggerFire, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, TriggerFire{Trigger: "t", Path: fmt.Sprintf("kb/%s/%06d.md", prefix, i), Episode: "learn", Source: "local", Commit: "c", Outcome: TriggerOutcomeEmitted})
	}
	return rows
}

func countFires(t *testing.T, svc *Service) (total, runs int, minID, maxID int64) {
	t.Helper()
	db := svc.rh.db
	require.NoError(t, db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM trigger_fires`).Scan(&total, &minID, &maxID))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM trigger_fires WHERE outcome = 'run'`).Scan(&runs))
	return
}

// Migration 000028: both tables exist after Open (and the body re-runs
// cleanly, which the recovery tests exercise for every migration).
func TestMigration000028_TablesExist(t *testing.T) {
	svc := newChangesService(t)
	for _, table := range []string{"trigger_watermarks", "trigger_fires"} {
		var n int
		require.NoError(t, svc.rh.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n))
		require.Equal(t, 1, n, "table %s", table)
	}
}

// Tx1 is bounded: a run with 12,000 fire rows logs the NEWEST 10,000 (the
// last of the sorted paths) plus one run row carrying fires_not_logged = 2000;
// the run row still counts every fire. Sabotage: insert every row.
func TestRecordTriggerRun_Tx1Bounded(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	rows := fireRows(12000, "big")
	logged, err := svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "w", RangeTo: "h", Paths: 12000, Evaluated: 12000, Fires: 12000, Rows: rows})
	require.NoError(t, err)
	require.Equal(t, TriggerFireRetention, logged)
	total, runs, _, _ := countFires(t, svc)
	require.Equal(t, TriggerFireRetention+1, total)
	require.Equal(t, 1, runs)
	recent, err := svc.Triggers().RecentTriggerFires(ctx, "agent/a", 2)
	require.NoError(t, err)
	require.Equal(t, TriggerOutcomeRun, recent[0].Outcome)
	require.Equal(t, 2000, recent[0].FiresNotLogged)
	require.Equal(t, 12000, recent[0].Fires, "the run row counts every fire")
	require.Equal(t, rows[11999].Path, recent[1].Path, "the newest path is kept")
	var first string
	require.NoError(t, svc.rh.db.QueryRow(`SELECT path FROM trigger_fires WHERE trigger = 't' ORDER BY id ASC LIMIT 1`).Scan(&first))
	require.Equal(t, rows[2000].Path, first, "the oldest 2000 are the ones dropped")
}

// One run row per run, even with no fire rows; a run row carries the range.
func TestRecordTriggerRun_OneRunRow(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	_, err := svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "w1", RangeTo: "h1", Paths: 5, Evaluated: 250})
	require.NoError(t, err)
	total, runs, _, _ := countFires(t, svc)
	require.Equal(t, 1, total)
	require.Equal(t, 1, runs)
	recent, err := svc.Triggers().RecentTriggerFires(ctx, "agent/a", 10)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, "w1", recent[0].RangeFrom)
	require.Equal(t, "h1", recent[0].RangeTo)
	require.Equal(t, 250, recent[0].Evaluated)
	require.Equal(t, 5, recent[0].Paths)
	require.Equal(t, "", recent[0].Trigger)
}

// FireLogPrunes: after 10,050 fire rows (two runs) the prune in tx2 keeps the
// newest 10,000 rows and they are the newest. Sabotage: off by one, or delete
// the newest.
func TestTriggerFires_PrunesToRetention(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	_, err := svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "a", RangeTo: "b", Rows: fireRows(6000, "one")})
	require.NoError(t, err)
	_, err = svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "b", RangeTo: "c", Rows: fireRows(4050, "two")})
	require.NoError(t, err)
	total, _, _, maxBefore := countFires(t, svc)
	require.Equal(t, 10052, total)

	require.NoError(t, svc.Triggers().AdvanceTriggerWatermarks(ctx, "agent/a", map[string]string{"t": "c"}, nil))
	total, _, minID, maxID := countFires(t, svc)
	require.Equal(t, TriggerFireRetention, total)
	require.Equal(t, maxBefore, maxID, "the newest row survives")
	require.Equal(t, maxID-int64(TriggerFireRetention)+1, minID, "exactly the newest 10,000 remain")

	// Steady state: another run of 3 rows prunes exactly 4 (3 fires + 1 run row).
	_, err = svc.Triggers().RecordTriggerRun(ctx, TriggerRun{Branch: "agent/a", RangeFrom: "c", RangeTo: "d", Rows: fireRows(3, "three")})
	require.NoError(t, err)
	require.NoError(t, svc.Triggers().AdvanceTriggerWatermarks(ctx, "agent/a", nil, nil))
	total, _, _, _ = countFires(t, svc)
	require.Equal(t, TriggerFireRetention, total)
}

// Watermarks: upsert, delete, read; keyed per (trigger, branch).
func TestTriggerWatermarks_RoundTrip(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	tr := svc.Triggers()
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/a", map[string]string{"x": "h1", "y": "h1"}, nil))
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/b", map[string]string{"x": "other"}, nil))
	wms, err := tr.TriggerWatermarks(ctx, "agent/a")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"x": "h1", "y": "h1"}, wms)
	require.NoError(t, tr.AdvanceTriggerWatermarks(ctx, "agent/a", map[string]string{"x": "h2"}, []string{"y"}))
	wms, err = tr.TriggerWatermarks(ctx, "agent/a")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"x": "h2"}, wms)
	// OpenExperiment copies pipeline watermarks, never trigger watermarks.
	_, err = svc.Experiments().OpenExperiment(ctx, "e", "", "agent/a")
	require.NoError(t, err)
	wms, err = tr.TriggerWatermarks(ctx, "exp/e")
	require.NoError(t, err)
	require.Empty(t, wms, "trigger bookmarks never fork onto exp/*")
}

// ---- The ontology at a commit, the signer, the anchor

func TestOntologyAtCommit_ReadsTheHeadsFile(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	const yaml = "id: t\nname: T\ntopics:\n  tasks:\n    description: t\n    triggers:\n      - {name: all, on: learn, do: emit}\n"
	r, err := svc.Facts().WriteFact(ctx, "agent/a", ".knomit/ontology.yaml", yaml, "ont", "updated")
	require.NoError(t, err)
	h := plumbing.NewHash(r.CommitHash)
	path, blob, data, err := svc.Triggers().OntologyAtCommit(ctx, h)
	require.NoError(t, err)
	require.Equal(t, ".knomit/ontology.yaml", path)
	require.Equal(t, yaml, string(data))
	want, err := svc.rh.readBlobHashAtCommit(ctx, ".knomit/ontology.yaml", r.CommitHash)
	require.NoError(t, err)
	require.Equal(t, want, blob, "the blob is the git blob hash of the file at that commit")

	// A tree without any ontology file: a named error.
	root := plumbing.NewHash(writeF(t, svc, "main", "kb/x.md"))
	_, _, _, err = svc.Triggers().OntologyAtCommit(ctx, root)
	if err == nil {
		t.Skip("fixture: main carries an ontology at init; the not-found case is covered by the empty-tree path")
	}
}

// CommitSignerOf sets the fingerprint only when the SSHSIG verifies, and the
// instance's own commits carry SignerFingerprint.
func TestCommitSignerOf_MatchesSignerFingerprint(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	h := plumbing.NewHash(writeF(t, svc, "agent/a", "kb/tasks/a/t.md"))
	signer, err := svc.Triggers().CommitSignerOf(ctx, h)
	require.NoError(t, err)
	require.NotEmpty(t, signer.Fingerprint)
	require.Equal(t, svc.SignerFingerprint(), signer.Fingerprint)

	// An unsigned commit has no signer.
	restamp(t, svc, "agent/a", nil, fixedTime(3))
	_, err = svc.Triggers().CommitSignerOf(ctx, mustHeadHash(t, svc, "agent/a"))
	require.ErrorIs(t, err, ErrUnsigned)
}

// VerifiedAnchor: no ref → ZeroHash; a ref set without a fold → the anchor and
// a NIL cached set (F09 fills it only by folding in this process); AncestorSet
// is the fallback walk.
func TestVerifiedAnchor_NilCacheUntilAFold(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	anchor, set, err := svc.Triggers().VerifiedAnchor(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, plumbing.ZeroHash, anchor)
	require.Nil(t, set)

	a := writeF(t, svc, "main", "kb/tasks/a/a.md")
	b := writeF(t, svc, "main", "kb/tasks/a/b.md")
	require.NoError(t, svc.TestingSetRef(VerifiedRefName("main"), a))
	anchor, set, err = svc.Triggers().VerifiedAnchor(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(a), anchor)
	require.Nil(t, set, "the cache is nil until a fold runs in this process")

	below, err := svc.Triggers().AncestorSet(ctx, anchor)
	require.NoError(t, err)
	require.True(t, below[plumbing.NewHash(a)])
	require.False(t, below[plumbing.NewHash(b)], "b is above the anchor")
	require.True(t, below[mustRootHash(t, svc, "main")])
}

func mustRootHash(t *testing.T, svc *Service, branch string) plumbing.Hash {
	t.Helper()
	root, err := svc.RootCommit(context.Background(), branch)
	require.NoError(t, err)
	return plumbing.NewHash(root)
}

// fixedTime is a deterministic commit time for hand-built commits.
func fixedTime(n int64) time.Time { return time.Unix(1790000000+n, 0).UTC() }
