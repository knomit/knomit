package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

func TestBranchID_MissingBranch_WrapsErrBranchNotFound(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	_, err = svc.rh.branchID(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatalf("expected error for missing branch, got nil")
	}
	if !errors.Is(err, ErrBranchNotFound) {
		t.Errorf("error should wrap ErrBranchNotFound, got: %v", err)
	}
}

// TestLockBranch_DeferReleasesOnPanic pins the panic-safety contract that
// reconcileNow's IIFE (`defer rh.lockBranch(x)()`) relies on: if the body
// holding the lock panics, the deferred unlock still fires. The prior
// non-deferred `unlock := rh.lockBranch(x); ...; unlock()` form would have
// leaked the lock on panic, deadlocking every subsequent op on that branch.
func TestLockBranch_DeferReleasesOnPanic(t *testing.T) {
	rh := &repoHandler{}

	func() {
		defer func() { _ = recover() }()
		defer rh.lockBranch("main")()
		panic("simulated reconcileMain panic")
	}()

	// If the deferred unlock did not fire, the second acquire blocks forever.
	done := make(chan struct{})
	go func() {
		defer rh.lockBranch("main")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("lockBranch deferred unlock did not release on panic — second acquire blocked")
	}
}
