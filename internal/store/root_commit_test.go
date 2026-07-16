package store

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootCommit_StableAcrossBranchesAndCalls(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ctx := context.Background()
	root, err := svc.RootCommit(ctx, "agent/test")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), root)

	// The root is a property of the repo, not the branch: main and the agent
	// branch share the init commit.
	rootMain, err := svc.RootCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, root, rootMain)

	// Stable across calls.
	again, err := svc.RootCommit(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, root, again)
}

func TestRootCommit_UnknownBranchErrors(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.RootCommit(context.Background(), "no-such-branch")
	require.Error(t, err)
}
