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

// TestRootCommit_DistinctAcrossRepos pins the nonce's actual purpose: two repos
// created back-to-back (same wall-second is likely, and is exactly the collision
// the knomit-repo-nonce guards against) must have DISTINCT root commits. Every
// other input to the init commit is fixed — manifest body, signature, and a
// second-precision timestamp — so the nonce in the init commit message (repo.go)
// is the only entropy source. A refactor dropping the nonce collides same-second
// repos and must fail here.
func TestRootCommit_DistinctAcrossRepos(t *testing.T) {
	newRepo := func() *Service {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
		return svc
	}

	ctx := context.Background()
	// Created back-to-back to maximize the same-wall-second overlap the nonce
	// exists to survive.
	svc1 := newRepo()
	svc2 := newRepo()

	root1, err := svc1.RootCommit(ctx, "agent/test")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), root1)

	root2, err := svc2.RootCommit(ctx, "agent/test")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), root2)

	require.NotEqual(t, root1, root2,
		"root commits of independently created repos must differ — the nonce is the only entropy")
}

func TestRootCommit_UnknownBranchErrors(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.RootCommit(context.Background(), "no-such-branch")
	require.Error(t, err)
}
