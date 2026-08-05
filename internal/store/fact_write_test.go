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
