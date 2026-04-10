package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestDropBranch_RemovesGitRef asserts that DropBranch deletes the git ref
// in addition to the SQLite rows. Before commit f9ef0f9 (predecessor fix),
// DropBranch only deleted the SQLite side, leaving the git ref intact —
// which caused Verify's branches-table check to fire because
// listBranchRefsForVerify would enumerate the orphaned ref without
// finding a matching branches row.
func TestDropBranch_RemovesGitRef(t *testing.T) {
	t.Log("Scenario: CreateBranch(feature, main), DropBranch(feature), expect git ref gone")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	// Confirm the ref exists before the drop.
	_, err = svc.rh.gits.Reference(plumbing.NewBranchReferenceName("feature"))
	require.NoError(t, err, "feature ref should exist after CreateBranch")

	require.NoError(t, svc.Branches().DropBranch(context.Background(), "feature"))

	// After the drop, the ref must be gone.
	_, err = svc.rh.gits.Reference(plumbing.NewBranchReferenceName("feature"))
	require.Error(t, err, "feature ref should be gone after DropBranch")

	// And Verify must stay clean — this is the end-to-end assertion.
	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "post-drop repo must be clean: %v", report.Issues)
}

// TestDropBranch_DoesNotAffectOtherBranches asserts that dropping a
// branch leaves siblings untouched.
func TestDropBranch_DoesNotAffectOtherBranches(t *testing.T) {
	t.Log("Scenario: create feature from main with a fact, drop feature, main's HEAD and fact unchanged")
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	// Write a fact on main so there's content to diff against.
	mainFact := "---\ntype: observation\nconfidence: 0.5\nsources: 1\ndomain: [x]\nentities: []\nrefs: []\n---\n# main fact\n\nbody\n"
	_, err = svc.Facts().WriteFact(context.Background(), "main", "kb/main.md", mainFact, "add main fact", "test")
	require.NoError(t, err)

	mainHeadBefore, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)

	// Create feature, write a fact to it.
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))
	featureFact := "---\ntype: observation\nconfidence: 0.5\nsources: 1\ndomain: [x]\nentities: []\nrefs: []\n---\n# feature fact\n\nbody\n"
	_, err = svc.Facts().WriteFact(context.Background(), "feature", "kb/feature.md", featureFact, "add feature fact", "test")
	require.NoError(t, err)

	// Drop feature.
	require.NoError(t, svc.Branches().DropBranch(context.Background(), "feature"))

	// main's HEAD must be unchanged.
	mainHeadAfter, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, mainHeadBefore, mainHeadAfter, "main HEAD must not move when feature is dropped")

	// main's fact must still be readable.
	res, err := svc.Facts().ReadFact(context.Background(), "main", "kb/main.md", nil)
	require.NoError(t, err)
	require.Contains(t, res.Content, "# main fact")

	// Verify clean.
	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "integrity issues: %v", report.Issues)
}
