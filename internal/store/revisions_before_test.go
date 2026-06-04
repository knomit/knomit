package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeThreeVersions writes kb/t.md three times and returns the three commit
// hashes (oldest → newest).
func writeThreeVersions(t *testing.T, svc *Service, ctx context.Context, branch string) (c1, c2, c3 string) {
	t.Helper()
	r1, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v1", 0.9, nil), "create t", "")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v2", 0.8, nil), "edit t", "")
	require.NoError(t, err)
	r3, err := svc.Facts().WriteFact(ctx, branch, "kb/t.md", testFactBody("v3", 0.7, nil), "edit t again", "")
	require.NoError(t, err)
	return r1.CommitHash, r2.CommitHash, r3.CommitHash
}

// TestRevisionsBefore_WalksAncestryNewestFirst writes three versions across
// three commits and asserts RevisionsBefore returns them newest→oldest with
// commit metadata populated.
func TestRevisionsBefore_WalksAncestryNewestFirst(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	c1, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c3, 10)
	require.NoError(t, err)
	require.Len(t, revs, 3)
	require.Equal(t,
		[]string{c3, c2, c1},
		[]string{revs[0].Commit, revs[1].Commit, revs[2].Commit},
		"revisions must be newest → oldest")
	require.Equal(t, "edit t again", revs[0].Message)
	require.Equal(t, "create t", revs[2].Message)
	require.Equal(t, "added", revs[2].Action, "oldest revision is the creation")
	require.Equal(t, "modified", revs[0].Action)
	require.NotZero(t, revs[0].CommittedAt, "CommittedAt populated from commit_log")
}

// TestRevisionsBefore_BoundedToAnchor pins that history is the ancestry of the
// anchor commit — edits made after the anchor are not surfaced.
func TestRevisionsBefore_BoundedToAnchor(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	c1, c2, _ := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c2, 10)
	require.NoError(t, err)
	require.Len(t, revs, 2, "anchoring at c2 must exclude the c3 edit made after it")
	require.Equal(t, []string{c2, c1}, []string{revs[0].Commit, revs[1].Commit})
}

// TestRevisionsBefore_RespectsLimit pins that limit caps the returned count
// (newest-first).
func TestRevisionsBefore_RespectsLimit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	_, c2, c3 := writeThreeVersions(t, svc, ctx, "main")

	revs, err := svc.Search().RevisionsBefore(ctx, "main", "kb/t.md", c3, 2)
	require.NoError(t, err)
	require.Len(t, revs, 2)
	require.Equal(t, []string{c3, c2}, []string{revs[0].Commit, revs[1].Commit})
}
