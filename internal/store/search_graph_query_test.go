package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestIncomingAtCommit_TwoSourceVersions: spec test scenario 1.
// D@c1 ref's E and D@c2 still ref's E (E unchanged). Incoming for E@c0
// returns both, each anchored to its own source commit.
func TestIncomingAtCommit_TwoSourceVersions(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c0Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d v2", 0.85, []string{"kb/e.md"}), "d v2→e", "")
	require.NoError(t, err)

	got, err := svc.Search().IncomingAtCommit(ctx, branch, "kb/e.md", c0Res.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 2)

	commits := []string{got[0].Commit, got[1].Commit}
	require.ElementsMatch(t, []string{c1Res.CommitHash, c2Res.CommitHash}, commits)
	require.Equal(t, "kb/d.md", got[0].Path)
	require.Equal(t, "kb/d.md", got[1].Path)

	// Each candidate's CommittedAt must be populated from commit_log via the
	// LEFT JOIN in the post-cypher SQL filter.
	require.NotZero(t, got[0].CommittedAt, "CommittedAt should be populated from commit_log")
	require.NotZero(t, got[1].CommittedAt, "CommittedAt should be populated from commit_log")
}

// TestOutgoingAtCommit_WalksBackSparseHistory regresses the bug where
// OutgoingAtCommit filtered edges by exact-match on source_commit. The
// graph stores facts SPARSELY — a fact added at c1 with no subsequent
// edits has exactly one stored revision (c1) yet is semantically present
// at every commit between c1 and HEAD. A query "outgoing for path P as
// of commit Q" must resolve P's effective write-commit ≤ Q first, then
// filter edges by source_commit = effective_commit. Exact-match on Q
// alone returns 0 whenever Q ≠ P's write-commit.
func TestOutgoingAtCommit_WalksBackSparseHistory(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// c0: create the ref target.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	// c1: create the source fact with a ref to e.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)
	// c2: write an UNRELATED fact, advancing the branch tip.
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/unrelated.md", testFactBody("u", 0.5, nil), "init unrelated", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash

	// Query outgoing for kb/d.md AS OF the branch tip c2 — note that d's
	// effective write-commit is c1, not c2. Exact-match query returns 0;
	// the correct behavior is to walk back from c2 to find d's last write
	// (c1) and surface its edges.
	got, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/d.md", c2)
	require.NoError(t, err)
	require.Len(t, got, 1, "outgoing must walk back to d's last write to surface its edges")
	require.Equal(t, "kb/e.md", got[0].Path)
}

// TestIncomingAtCommit_WalksBackSparseHistory: mirror of the above for
// the incoming side. E was last written at c0; a query at a later
// branch tip must still surface incoming edges to it.
func TestIncomingAtCommit_WalksBackSparseHistory(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// c0: create the target.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	// c1: source ref's e.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)
	// c2: unrelated write to advance the branch tip.
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/unrelated.md", testFactBody("u", 0.5, nil), "init unrelated", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash

	// Query incoming for kb/e.md AS OF c2 (e was last written at c0).
	got, err := svc.Search().IncomingAtCommit(ctx, branch, "kb/e.md", c2)
	require.NoError(t, err)
	require.Len(t, got, 1, "incoming must walk back to e's last write to surface edges into it")
	require.Equal(t, "kb/d.md", got[0].Path)
}

// TestOutgoingAtCommit returns the outgoing refs of (path, commit_hash)
// using edge.source_commit = commit_hash.
func TestOutgoingAtCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c0Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)

	got, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/d.md", c1Res.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "kb/e.md", got[0].Path)
	require.Equal(t, c0Res.CommitHash, got[0].Commit)
}

// TestExplainFact_MatchesIncomingAtCommit_AtHEAD verifies design scenario 6:
// HEAD ExplainFact returns the same incoming items as IncomingAtCommit
// invoked with the branch's HEAD-active commit for the path.
func TestExplainFact_MatchesIncomingAtCommit_AtHEAD(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c0Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d1.md", testFactBody("d1", 0.8, []string{"kb/e.md"}), "d1→e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d2.md", testFactBody("d2", 0.7, []string{"kb/e.md"}), "d2→e", "")
	require.NoError(t, err)

	headExplain, err := svc.Search().ExplainFact(ctx, branch, "kb/e.md")
	require.NoError(t, err)

	atC0, err := svc.Search().IncomingAtCommit(ctx, branch, "kb/e.md", c0Res.CommitHash)
	require.NoError(t, err)

	require.ElementsMatch(t, refSummaryPaths(headExplain.Incoming), refSummaryPaths(atC0))
}

func refSummaryPaths(rs []RefSummary) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}

// TestIncomingAtCommit_PopulatesType verifies the type of the source fact is
// returned on each RefSummary so the UI can color-code chips by epistemic type.
func TestIncomingAtCommit_PopulatesType(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c0Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md",
		testFactBodyWithType("e", 0.9, nil, fact.Concept), "init e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/p.md",
		testFactBodyWithType("p", 0.8, []string{"kb/e.md"}, fact.Principle), "p→e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/h.md",
		testFactBodyWithType("h", 0.7, []string{"kb/e.md"}, fact.Hypothesis), "h→e", "")
	require.NoError(t, err)

	got, err := svc.Search().IncomingAtCommit(ctx, branch, "kb/e.md", c0Res.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 2)

	byPath := map[string]string{}
	for _, rs := range got {
		byPath[rs.Path] = rs.Type
	}
	require.Equal(t, string(fact.Principle), byPath["kb/p.md"])
	require.Equal(t, string(fact.Hypothesis), byPath["kb/h.md"])
}

// TestExplainFact_RetractedAtHEAD: when the path has been retracted from HEAD
// (its branch_facts row is gone), ExplainFact returns ErrFactNotLive instead
// of a wrapped sql.ErrNoRows. Handlers map this sentinel to 404.
func TestExplainFact_RetractedAtHEAD(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Create a fact, then retract it. The branch_facts row is removed at delete.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/gone.md", testFactBody("gone", 0.9, nil), "init", "")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/gone.md", "retract gone")
	require.NoError(t, err)

	_, err = svc.Search().ExplainFact(ctx, branch, "kb/gone.md")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrFactNotLive),
		"expected ErrFactNotLive for retracted path, got %v", err)
}

// TestExplainFact_NeverIndexed: a path that has never been written returns
// ErrFactNotLive too — same code path, no branch_facts row.
func TestExplainFact_NeverIndexed(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.Search().ExplainFact(context.Background(), "main", "kb/never.md")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrFactNotLive),
		"expected ErrFactNotLive for never-indexed path, got %v", err)
}

// TestOutgoingAtCommit_DropsMissingCommitLog: when a target_commit is not
// in commit_log (e.g. GC'd or never indexed), OutgoingAtCommit drops the
// entry rather than returning it with CommittedAt=0 — its self-link would
// 404 and the UI has no way to distinguish a valid entry from a stale one.
func TestOutgoingAtCommit_DropsMissingCommitLog(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c0Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)

	// Sanity: baseline returns the outgoing ref with CommittedAt populated.
	got, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/d.md", c1Res.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotZero(t, got[0].CommittedAt)

	// Remove the target's commit_log row to simulate a missing entry.
	_, err = svc.rh.db.ExecContext(ctx,
		`DELETE FROM commit_log WHERE commit_hash = ?`, c0Res.CommitHash)
	require.NoError(t, err)

	got, err = svc.Search().OutgoingAtCommit(ctx, branch, "kb/d.md", c1Res.CommitHash)
	require.NoError(t, err)
	require.Empty(t, got,
		"OutgoingAtCommit must drop entries whose target_commit is not in commit_log")
}

// TestOutgoingAtCommit_PopulatesType verifies the type of the target fact is
// returned on each RefSummary.
func TestOutgoingAtCommit_PopulatesType(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md",
		testFactBodyWithType("e", 0.9, nil, fact.Synthesis), "init e", "")
	require.NoError(t, err)
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md",
		testFactBodyWithType("d", 0.8, []string{"kb/e.md"}, fact.Pattern), "d→e", "")
	require.NoError(t, err)

	got, err := svc.Search().OutgoingAtCommit(ctx, branch, "kb/d.md", c1Res.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "kb/e.md", got[0].Path)
	require.Equal(t, string(fact.Synthesis), got[0].Type)
}

// TestIncomingAtCommit_IncludesRetractedSource regresses the bug where
// IncomingAtCommit dropped any edge whose source Fact node was tombstoned
// (s.deleted = true). In the UI this looked like "this fact has no incoming
// edges" even when another (now-retracted) fact had clearly referenced it.
// The retracted state must be exposed via RefSummary.Deleted, not hidden.
func TestIncomingAtCommit_IncludesRetractedSource(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	aRes, err := svc.Facts().WriteFact(ctx, branch, "kb/a.md", testFactBody("a", 0.9, nil), "init a", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/b.md", testFactBody("b", 0.8, []string{"kb/a.md"}), "init b →a", "")
	require.NoError(t, err)
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/b.md", "retract b")
	require.NoError(t, err)

	got, err := svc.Search().IncomingAtCommit(ctx, branch, "kb/a.md", aRes.CommitHash)
	require.NoError(t, err)
	require.Len(t, got, 1, "incoming edge from retracted source must still be returned")
	require.Equal(t, "kb/b.md", got[0].Path)
	require.True(t, got[0].Deleted, "Deleted must reflect that the source fact is retracted")
}

// TestOutgoingAtCommit_FiltersStaleSelfLoop covers defense-in-depth: even
// though resolveTargetCommit now resolves self-refs to the previous version
// (preventing new self-loops), legacy data may still contain edges where
// source and target are the same (path, commit) tuple. Such edges navigate
// to nowhere and must be hidden on read.
func TestOutgoingAtCommit_FiltersStaleSelfLoop(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	xRes, err := svc.Facts().WriteFact(ctx, branch, "kb/x.md", testFactBody("x", 0.9, nil), "init x", "")
	require.NoError(t, err)

	si := svc.si

	// Bypass the write-path fix to inject a stale self-loop edge directly.
	nodeID, err := si.graphNodeIDByBlob(ctx, "kb/x.md", xRes.BlobHash)
	require.NoError(t, err)
	require.NotZero(t, nodeID, "graph node for the written fact must exist")

	edgeID, err := si.graphInsertEdgeReturningID(ctx, nodeID, nodeID, EdgeDerivedFrom)
	require.NoError(t, err)
	require.NoError(t, si.graphSetEdgeProps(ctx, edgeID, map[string]string{
		"source_commit": xRes.CommitHash,
		"target_commit": xRes.CommitHash,
	}))

	out, err := svc.gq.OutgoingAtCommit(ctx, branch, "kb/x.md", xRes.CommitHash)
	require.NoError(t, err)
	require.Empty(t, out, "stale self-loop edge (same path & commit both sides) must be filtered from outgoing")

	in, err := svc.gq.IncomingAtCommit(ctx, branch, "kb/x.md", xRes.CommitHash)
	require.NoError(t, err)
	require.Empty(t, in, "stale self-loop edge must be filtered from incoming")
}
