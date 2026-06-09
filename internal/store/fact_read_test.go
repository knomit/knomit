package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadFact_ReturnsErrPathNotFound_HEAD: a path that has never been written
// surfaces ErrPathNotFound from HEAD-anchored reads. Handlers rely on this to
// distinguish "no fact at this path" (404) from a real store error (500).
func TestReadFact_ReturnsErrPathNotFound_HEAD(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.Facts().ReadFact(context.Background(), "main", "kb/missing.md", nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"expected ErrPathNotFound for never-written path at HEAD, got %v", err)
}

// TestReadFact_ReturnsErrPathNotFound_AtCommit: a path missing at a specific
// commit surfaces ErrPathNotFound from commit-anchored reads.
func TestReadFact_ReturnsErrPathNotFound_AtCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	c0, err := svc.Facts().WriteFact(ctx, "main", "kb/e.md", testFactBody("e", 0.9, nil), "init", "")
	require.NoError(t, err)

	_, err = svc.Facts().ReadFact(ctx, "main", "kb/never.md", &ReadFactOpts{AtCommit: c0.CommitHash})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"expected ErrPathNotFound for missing path at commit, got %v", err)
}

// TestReadFact_BeforeCommit_NoPrior_ReturnsErrPathNotFound: when the path has
// no prior version before the requested commit, the fallback path returns
// ErrPathNotFound rather than a free-form string error.
func TestReadFact_BeforeCommit_NoPrior_ReturnsErrPathNotFound(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	// Write a fact then read with BeforeCommit pointing at its own commit:
	// there is no prior version, so the log walk yields nothing.
	c0, err := svc.Facts().WriteFact(ctx, "main", "kb/e.md", testFactBody("e", 0.9, nil), "init", "")
	require.NoError(t, err)

	_, err = svc.Facts().ReadFact(ctx, "main", "kb/e.md", &ReadFactOpts{BeforeCommit: c0.CommitHash})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"expected ErrPathNotFound for path with no prior version, got %v", err)
}

// TestReadFact_HappyPath_ReturnsContent: sanity that the path-not-found
// detection doesn't break the success case.
func TestReadFact_HappyPath_ReturnsContent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/e.md", testFactBody("e", 0.9, nil), "init", "")
	require.NoError(t, err)

	res, err := svc.Facts().ReadFact(ctx, "main", "kb/e.md", nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	require.False(t, errors.Is(err, ErrPathNotFound))
}
