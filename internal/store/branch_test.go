package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func TestResolveRef_MissingBranch_WrapsErrBranchNotFound(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.rh.resolveRef(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatalf("expected error for missing ref, got nil")
	}
	if !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("error should wrap ErrBranchNotFound, got: %v", err)
	}
	// Preserve the underlying go-git error for introspection.
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Errorf("error should also wrap plumbing.ErrReferenceNotFound, got: %v", err)
	}
}
