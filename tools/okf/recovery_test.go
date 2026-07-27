package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// headTarget reports the ref HEAD points at, whether or not it has a commit.
func headTarget(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.HEAD, false)
	require.NoError(t, err)
	require.Equal(t, plumbing.SymbolicReference, ref.Type())
	return ref.Target().Short()
}

func indexSize(t *testing.T, dir string) int {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	return len(idx.Entries)
}

// A `sync -b <new>` creates an orphan branch — moving HEAD and emptying the
// index — BEFORE the export that justifies it has produced a byte. When the
// export then fails, the repository must be left exactly as it was found: on
// its old branch, with its index intact and its bundle still on disk. Stranding
// the user on an unborn branch with an empty index gave them no way back that
// anything told them about.
func TestCLI_FailedOrphanSyncRestoresTheRepository(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	before := headTarget(t, outDir)
	entriesBefore := indexSize(t, outDir)
	require.NotZero(t, entriesBefore, "test premise: the clone staged a bundle")

	// Make the export fail after the branch has been prepared: an owned root
	// that is a symbolic link is refused by reconcile, which runs downstream of
	// checkoutOutputBranch.
	require.NoError(t, os.RemoveAll(filepath.Join(outDir, "views")))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(outDir, "views")))

	// The source branch must exist so the failure is the reconcile, not a
	// missing branch.
	kbRepo, err := git.PlainOpen(kbDir)
	require.NoError(t, err)
	head, err := kbRepo.Head()
	require.NoError(t, err)
	require.NoError(t, kbRepo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("other"), head.Hash())))

	var buf bytes.Buffer
	err = runSync([]string{"-b", "other"}, outDir, &buf)
	require.Error(t, err, "the symlinked owned root must be refused")

	require.Equal(t, before, headTarget(t, outDir),
		"HEAD must be back on the branch the run started from")
	require.Equal(t, entriesBefore, indexSize(t, outDir),
		"the index must be restored, not left empty")
	require.FileExists(t, filepath.Join(outDir, "index.md"),
		"the previous bundle must still be on disk")
	require.Contains(t, buf.String(), "nothing was committed",
		"and the user must be told the repository was left as found")
}

// An owned root that is a symbolic link would carry every write in reconcile
// outside the repository — `ln -s /etc kb` writes into /etc — and the prune
// would then remove the link and orphan what it wrote.
func TestReconcile_RefusesASymlinkedOwnedRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "kb")))

	_, _, err := reconcile(dir, map[string][]byte{"kb/a/x.md": []byte("hi")})
	require.Error(t, err)
	require.ErrorContains(t, err, "symbolic link")
	require.NoFileExists(t, filepath.Join(outside, "a", "x.md"),
		"nothing may be written through the link")
}

// sync and branches must find the export from anywhere inside it, as git does.
// PlainOpen does not search upward, so both failed from a subdirectory with
// "run knomit-okf clone first" — advice that is wrong for the one user who did.
func TestCLI_SyncAndBranchesWorkFromASubdirectory(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	sub := filepath.Join(outDir, "kb", "decisions")
	require.DirExists(t, sub, "test premise: the bundle has a subdirectory")

	var buf bytes.Buffer
	require.NoError(t, runSync(nil, sub, &buf))
	require.Contains(t, buf.String(), "already up to date")
	require.Equal(t, 1, commitCount(t, outDir), "no second commit, and none in the subdirectory")
	require.NoFileExists(t, filepath.Join(sub, configFile),
		"the bundle must not be re-rendered into the subdirectory")

	buf.Reset()
	require.NoError(t, runBranches(nil, sub, &buf))
	require.Contains(t, buf.String(), "up to date")
}

// Searching upward means a stray sync inside an UNRELATED repository now finds
// one. It must refuse rather than render a bundle into someone's source tree.
func TestCLI_SyncRefusesARepositoryItDidNotCreate(t *testing.T) {
	kbDir, _ := newKB(t)
	other := t.TempDir()
	_, err := git.PlainInit(other, false)
	require.NoError(t, err)

	var buf bytes.Buffer
	err = runSync([]string{"--source", kbDir}, other, &buf)
	require.Error(t, err)
	require.ErrorContains(t, err, "not a knomit-okf export")
	require.NoFileExists(t, filepath.Join(other, "index.md"))
}

// HEAD pointing at a branch with no commit is the same state prepareOrphanBranch
// creates, so it must get the same preparation. Skipping it left whatever the
// previous branch had staged to be committed as the new branch's first commit.
func TestCheckoutOutputBranch_UnbornHEADStillClearsTheIndex(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	require.NotZero(t, indexSize(t, outDir))

	// Point HEAD at a branch that does not exist, leaving the index populated —
	// what `git checkout --orphan` does, and what an interrupted run leaves.
	require.NoError(t, setHEADTo(repo, "fresh"))

	created, rollback, err := checkoutOutputBranch(repo, outDir, "fresh")
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, rollback, "an unborn HEAD is recoverable too")
	require.Zero(t, indexSize(t, outDir),
		"the index must be emptied so nothing is inherited into the first commit")
}
