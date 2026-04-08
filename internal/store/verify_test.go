package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerify_FreshRepoIsClean asserts that a freshly initialised store with
// no facts written reports IsClean() == true and zero issues.
func TestVerify_FreshRepoIsClean(t *testing.T) {
	t.Log("Scenario: open a fresh store, init repo, run Verify, expect clean report")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "fresh repo must be clean, got issues: %v", report.Issues)
	require.True(t, report.IsStrictlyClean(), "fresh repo must be strictly clean")
	require.Contains(t, report.Branches, "agent/test")
}

// TestVerify_DetectsMissingBlob asserts that deleting a blob object from the
// storer causes Verify to report a git-reachability Error naming the missing blob.
func TestVerify_DetectsMissingBlob(t *testing.T) {
	t.Log("Scenario: write a fact, delete its blob from the storer, expect git-reachability Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	res, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\nbody", "add x", "test")
	require.NoError(t, err)
	require.NotEmpty(t, res.BlobHash)

	// Delete the blob from the storer directly.
	require.NoError(t, svc.deleteObjectForTest(res.BlobHash))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean(), "deleted blob must produce an Error")
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryGitReachability && i.Severity == SeverityError && strings.Contains(i.Detail, res.BlobHash) {
			found = true
			break
		}
	}
	require.True(t, found, "expected git-reachability Error naming blob %s, got: %v", res.BlobHash, report.Issues)
}

// TestVerify_DetectsCommitLogGap asserts that removing a row from commit_log
// causes Verify to report a commit-log Error naming the missing commit.
func TestVerify_DetectsCommitLogGap(t *testing.T) {
	t.Log("Scenario: write two facts, delete second commit's commit_log row, expect commit-log Error")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	r1, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/x.md", "---\ntype: observation\n---\na", "add x", "test")
	require.NoError(t, err)
	r2, err := svc.Facts().WriteFact(context.Background(), "agent/test", "kb/y.md", "---\ntype: observation\n---\nb", "add y", "test")
	require.NoError(t, err)
	_ = r1

	require.NoError(t, svc.deleteCommitLogRowForTest(r2.CommitHash))

	report, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.False(t, report.IsClean())
	found := false
	for _, i := range report.Issues {
		if i.Category == CategoryCommitLog && i.Severity == SeverityError && strings.Contains(i.Detail, r2.CommitHash) {
			found = true
			break
		}
	}
	require.True(t, found, "expected commit-log Error naming commit %s, got: %v", r2.CommitHash, report.Issues)
}
