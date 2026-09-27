package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A root file's name is chosen for an EXTERNAL reader — git providers look for
// README.md. writeFile lowercases to keep fact topics free of case duplicates;
// that rule must not reach a file that is not a fact.
func TestWriteRootFile_PreservesCase(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	fi := svc.Facts()

	_, err = fi.WriteRootFile(context.Background(), "main",
		"README.md", "# hello", "docs: update README.md", "update")
	require.NoError(t, err)

	paths, err := fi.ListAll(context.Background(), "main")
	require.NoError(t, err)
	require.Contains(t, paths, "README.md")
	require.NotContains(t, paths, "readme.md")
}

// The case-preserving door is for ROOT files only. Letting it take a nested
// path would hand callers a way to bypass fact-path normalization entirely.
func TestWriteRootFile_RejectsNestedPath(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.Facts().WriteRootFile(context.Background(), "main",
		"kb/Architecture/X.md", "# x", "msg", "update")
	require.Error(t, err)
	require.Contains(t, err.Error(), "root-level")
}

// The existing fact path must still normalize — this is the regression guard
// for the split.
func TestWriteFact_StillLowercasesPath(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	fi := svc.Facts()

	_, err = fi.WriteFact(context.Background(), "main",
		"kb/Technology/Software/AbCdEf12.md", testFactBody("AbCdEf12", 0.9, nil), "msg", "learn")
	require.NoError(t, err)

	paths, err := fi.ListAll(context.Background(), "main")
	require.NoError(t, err)
	require.Contains(t, paths, "kb/technology/software/abcdef12.md")
}

// WriteFactIfUnchanged is a compare-and-swap: a writer holding a stale blob
// must be refused, not allowed to overwrite the write that landed after its
// read.
func TestWriteFactIfUnchanged_RefusesStaleBlob(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	fi := svc.Facts()
	ctx := context.Background()
	const path = ".knomit/jobs/x/y.md"

	_, err = fi.WriteFact(ctx, "main", path, "v1", "v1", "update")
	require.NoError(t, err)
	read, err := fi.ReadFact(ctx, "main", path, &ReadFactOpts{WithHash: true})
	require.NoError(t, err)

	_, err = fi.WriteFactIfUnchanged(ctx, "main", path, "v2", "v2", "update", read.BlobHash)
	require.NoError(t, err, "the blob is still the one read: the write proceeds")

	_, err = fi.WriteFactIfUnchanged(ctx, "main", path, "v3", "v3", "update", read.BlobHash)
	require.ErrorIs(t, err, ErrFactChanged, "the blob moved on: the stale writer is refused")

	now, err := fi.ReadFact(ctx, "main", path, nil)
	require.NoError(t, err)
	require.Equal(t, "v2", now.Content)
}
