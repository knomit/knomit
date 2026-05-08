package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
