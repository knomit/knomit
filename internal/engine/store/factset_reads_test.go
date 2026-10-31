package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/merge"
)

// The set-based reads of a streamed FactSet (index/factset_reads.go). Each
// test asserts the read reached its rows and WHICH values came back.

// VersionsByBlob reads the facts row of every (path, blob), and parses the
// blob for a version that has no facts row (an old version a rebuild never
// indexed); a blob in neither is absent.
//
// Sabotage: skip the parsed fallback, and the row-less version is missing.
func TestFactSetReads_VersionsByBlob(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, a1 := write(t, svc, "main", "kb/a.md", "A one")
	_, a2 := write(t, svc, "main", "kb/a.md", "A two", "kb/x.md")
	_, err := db.Exec(`DELETE FROM facts WHERE path = 'kb/a.md' AND blob_hash = ?`, a1)
	require.NoError(t, err)
	got, err := svc.FactQuery().VersionsByBlob(ctx, []index.VersionKey{{Path: "kb/a.md", Blob: a1}, {Path: "kb/a.md", Blob: a2},
		{Path: "kb/a.md", Blob: "0000000000000000000000000000000000000000"}, {Path: "kb/a.md", Blob: a2}})
	require.NoError(t, err)
	require.Len(t, got, 2)
	one, two := got[index.VersionKey{Path: "kb/a.md", Blob: a1}], got[index.VersionKey{Path: "kb/a.md", Blob: a2}]
	require.True(t, one.Parsed, "a1 has no facts row: parsed from its blob")
	require.Equal(t, "A one", one.Title)
	require.False(t, two.Parsed)
	require.Equal(t, "A two", two.Title)
	require.Equal(t, []string{"kb/x.md"}, two.Refs)
	require.Equal(t, a2, two.Blob)
}

// BlobsByHash reads objects by hash, batched; a hash that is not a blob is
// absent.
func TestFactSetReads_BlobsByHash(t *testing.T) {
	svc := newMergeTestStore(t)
	commit, b := write(t, svc, "main", "kb/b.md", "B")
	got, err := svc.FactQuery().BlobsByHash(context.Background(), []string{b, commit, b, ""})
	require.NoError(t, err)
	require.Len(t, got, 1, "the commit hash is not a blob")
	require.Contains(t, string(got[b]), "# B")
}

// CurrentBlobs is what the branch holds NOW: the blob and the commit that
// holds it, per path; a retracted path is absent.
func TestFactSetReads_CurrentBlobs(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	c1, r1 := write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S")
	_, err := svc.Facts().DeleteFact(ctx, "main", "kb/s.md", "retract s")
	require.NoError(t, err)
	got, err := svc.FactQuery().CurrentBlobs(ctx, "main", []string{"kb/r.md", "kb/s.md", "kb/none.md"})
	require.NoError(t, err)
	require.Equal(t, map[string]index.CurrentVersion{"kb/r.md": {Commit: c1, Blob: r1}}, got)
}

// LiveAt at the watermark is branch_facts (fast path); at an older commit it
// is the derived history (historical), which agrees with what branch_facts
// held then: the blob and the commit that WROTE it (a merge that carried it
// resolves to the writer, recordedCommit), no version before the path was
// written, none after a retraction, the new one after a re-add. Dates come
// from commit_log.
//
// Sabotage: return LiveChange's row instead of its entry, and the merged
// path's historical commit is the merge, not the fast path's.
func TestFactSetReads_LiveAt(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	hq := svc.HistoryQuery()
	cA1, a1 := write(t, svc, "main", "kb/a.md", "A")
	cA2, a2 := write(t, svc, "main", "kb/a.md", "A two")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	cM, m := write(t, svc, "feature", "kb/m.md", "M")
	write(t, svc, "main", "kb/o.md", "O")
	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	merged, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, "main", "kb/a.md", "retract a")
	require.NoError(t, err)
	retracted, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	cA3, a3 := write(t, svc, "main", "kb/a.md", "A three")

	wm, err := hq.Watermark(ctx, "main")
	require.NoError(t, err)
	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, head, wm, "the watermark is the indexed tip")

	paths := []string{"kb/a.md", "kb/m.md"}
	fast, hist, err := hq.LiveAt(ctx, "main", wm, paths)
	require.NoError(t, err)
	require.False(t, hist, "at the watermark: branch_facts")
	require.Equal(t, a3, fast["kb/a.md"].Blob)
	require.Equal(t, cA3, fast["kb/a.md"].Commit)
	require.Equal(t, m, fast["kb/m.md"].Blob)
	require.Equal(t, cM, fast["kb/m.md"].Commit, "branch_facts names the commit that wrote m, not the merge")
	require.True(t, fast["kb/m.md"].HasMeta)
	require.Equal(t, "M", fast["kb/m.md"].Meta.Title)
	require.Positive(t, fast["kb/m.md"].Date)

	atMerge, hist, err := hq.LiveAt(ctx, "main", merged, paths)
	require.NoError(t, err)
	require.True(t, hist, "below the watermark: the derived history")
	require.Equal(t, index.LiveVersion{Commit: cA2, Blob: a2, Meta: atMerge["kb/a.md"].Meta, HasMeta: true, Date: atMerge["kb/a.md"].Date}, atMerge["kb/a.md"])
	require.Equal(t, "A two", atMerge["kb/a.md"].Meta.Title)
	require.Equal(t, m, atMerge["kb/m.md"].Blob)
	require.Equal(t, cM, atMerge["kb/m.md"].Commit, "the historical read agrees with branch_facts: the writer, not the merge")
	require.Positive(t, atMerge["kb/m.md"].Date)

	atRetracted, hist, err := hq.LiveAt(ctx, "main", retracted, paths)
	require.NoError(t, err)
	require.True(t, hist)
	require.NotContains(t, atRetracted, "kb/a.md", "retracted on the first-parent line: no live version")
	require.Contains(t, atRetracted, "kb/m.md")

	atFirst, _, err := hq.LiveAt(ctx, "main", cA1, paths)
	require.NoError(t, err)
	require.Equal(t, a1, atFirst["kb/a.md"].Blob)
	require.NotContains(t, atFirst, "kb/m.md", "not written yet")
}

// CommitDates reads commit_log's (commit, path) primary key, batched.
func TestFactSetReads_CommitDates(t *testing.T) {
	svc := newMergeTestStore(t)
	c, _ := write(t, svc, "main", "kb/d.md", "D")
	before := time.Now().Add(-time.Hour).Unix()
	got, err := svc.HistoryQuery().CommitDates(context.Background(), []index.PathAtCommit{{Path: "kb/d.md", Commit: c}, {Path: "kb/none.md", Commit: c}, {Path: "kb/d.md", Commit: ""}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Greater(t, got[index.PathAtCommit{Path: "kb/d.md", Commit: c}], before)
}

// The LevelReader is one walk call's reads: Refs is the ref rows of a set of
// citing blobs in (source_blob, ord) order (the author's order); Versions is
// the metadata and commit date of target versions, parsed from the blob when
// a version has no facts row; Level is Refs, then Versions for only the
// targets the caller has not seen. RefTargetsOfBlobs is the same rows without
// target_commit. Before the backfill (sentinel unset, table empty) all of
// them read the same.
//
// Sabotage: order Refs by target_path, and b comes before z; make Level read
// the versions of known targets too, and z's version is read.
func TestFactSetReads_LevelReaderAndTargets(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cB, bBlob := write(t, svc, "main", "kb/b.md", "B")
	cZ, zBlob := write(t, svc, "main", "kb/z.md", "Z")
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "https://example.com/x", "kb/z.md", "kb/b.md", "kb://0123456789ab/kb/far.md")

	check := func(when string) {
		lv, err := svc.GraphStore().NewLevelReader(ctx)
		require.NoError(t, err)
		defer lv.Close()
		rows, err := lv.Refs([]string{fBlob, fBlob, ""})
		require.NoError(t, err)
		require.Equal(t, []index.LevelRow{
			{SourceBlob: fBlob, Ord: 1, Path: "kb/z.md", TargetBlob: zBlob, TargetCommit: cZ},
			{SourceBlob: fBlob, Ord: 2, Path: "kb/b.md", TargetBlob: bBlob, TargetCommit: cB},
			{SourceBlob: fBlob, Ord: 3, Repo: "0123456789ab", Path: "kb/far.md"},
		}, rows, "%s: the author's order", when)
		metas, parsed, err := lv.Versions([]index.TargetVersion{{Path: "kb/z.md", Blob: zBlob, Commit: cZ}, {Path: "kb/b.md", Blob: bBlob}})
		require.NoError(t, err)
		require.False(t, parsed)
		z := metas[index.VersionKey{Path: "kb/z.md", Blob: zBlob}]
		require.True(t, z.HasMeta, when)
		require.Equal(t, "Z", z.Meta.Title)
		require.Positive(t, z.Date, "dated at its commit")
		require.Zero(t, metas[index.VersionKey{Path: "kb/b.md", Blob: bBlob}].Date, "no commit asked: no date")
		lrows, lmetas, _, err := lv.Level([]string{fBlob}, func(p string) bool { return p == "kb/z.md" })
		require.NoError(t, err)
		require.Equal(t, rows, lrows)
		require.Len(t, lmetas, 1, "%s: only the unseen target's version is read", when)
		require.Equal(t, "B", lmetas[index.VersionKey{Path: "kb/b.md", Blob: bBlob}].Meta.Title)
		_, none, _, err := lv.Level([]string{fBlob}, nil)
		require.NoError(t, err)
		require.Nil(t, none, "a nil filter reads no versions")
		ts, err := svc.GraphStore().RefTargetsOfBlobs(ctx, []string{fBlob})
		require.NoError(t, err)
		require.Equal(t, []index.RefTarget{
			{SourceBlob: fBlob, Ord: 1, Path: "kb/z.md", TargetBlob: zBlob},
			{SourceBlob: fBlob, Ord: 2, Path: "kb/b.md", TargetBlob: bBlob},
			{SourceBlob: fBlob, Ord: 3, Repo: "0123456789ab", Path: "kb/far.md"},
		}, ts, when)
		require.NoError(t, svc.GraphStore().RefEdgesReadiness(ctx), when)
	}
	check("backfilled")
	_, err := db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	check("before the backfill, repo id resolved")

	// A version with no facts row is parsed from its blob, and dated.
	_, err = db.Exec(`DELETE FROM branch_facts WHERE path = 'kb/z.md'`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM facts WHERE path = 'kb/z.md'`)
	require.NoError(t, err)
	lv, err := svc.GraphStore().NewLevelReader(ctx)
	require.NoError(t, err)
	metas, parsed, err := lv.Versions([]index.TargetVersion{{Path: "kb/z.md", Blob: zBlob, Commit: cZ}})
	lv.Close()
	require.NoError(t, err)
	require.True(t, parsed)
	z := metas[index.VersionKey{Path: "kb/z.md", Blob: zBlob}]
	require.True(t, z.Meta.Parsed)
	require.Equal(t, "Z", z.Meta.Title)
	require.Positive(t, z.Date)

	// F19 (store level): before the backfill with the repo id unresolved,
	// the reference index is not ready.
	svc.gq.SetRootCommitForTest(func(context.Context, string) (string, error) { return "", errors.New("unresolved") })
	defer svc.gq.SetRootCommitForTest(nil)
	require.ErrorIs(t, svc.GraphStore().RefEdgesReadiness(ctx), index.ErrRefEdgesNotReady)
}

// F20 (store level): a ref whose target blob is missing from objects has no
// row in the LevelReader's Refs and in RefTargetsOfBlobs; the other refs keep
// theirs.
func TestFactSetReads_MissingTargetBlobHasNoRow(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, tBlob := write(t, svc, "main", "kb/t.md", "T")
	_, aBlob := write(t, svc, "main", "kb/a.md", "A")
	_, err := db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, tBlob)
	require.NoError(t, err)
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/t.md", "kb/a.md")
	lv, err := svc.GraphStore().NewLevelReader(ctx)
	require.NoError(t, err)
	defer lv.Close()
	rows, err := lv.Refs([]string{fBlob})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "kb/a.md", rows[0].Path)
	require.Equal(t, aBlob, rows[0].TargetBlob)
	require.Equal(t, 1, rows[0].Ord, "ord keeps the position in the refs list")
	ts, err := svc.GraphStore().RefTargetsOfBlobs(ctx, []string{fBlob})
	require.NoError(t, err)
	require.Equal(t, []index.RefTarget{{SourceBlob: fBlob, Ord: 1, Path: "kb/a.md", TargetBlob: aBlob}}, ts)
}
