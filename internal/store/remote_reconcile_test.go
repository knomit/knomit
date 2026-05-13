package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestConfigureRemote_WritesTwoRefspecs(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "agent/test"))

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	rc, ok := cfg.Remotes["origin"]
	require.True(t, ok, "origin remote must be configured")
	require.Len(t, rc.Fetch, 2, "must write two refspecs (main + agent)")

	got := make(map[string]bool, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		got[string(rs)] = true
	}
	require.True(t, got["+refs/heads/main:refs/remotes/origin/main"], "main refspec missing: %v", rc.Fetch)
	require.True(t, got["+refs/heads/agent/test:refs/remotes/origin/agent/test"], "agent refspec missing: %v", rc.Fetch)
}

// TestConfigureRemote_IsIdempotent covers the early-return path: calling
// configureRemote twice with identical args must leave exactly two refspecs
// (no duplication) and must not error.
func TestConfigureRemote_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "agent/test"))
	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "agent/test"))

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	rc, ok := cfg.Remotes["origin"]
	require.True(t, ok, "origin remote must be configured")
	require.Len(t, rc.Fetch, 2, "second call must not duplicate refspecs")
	require.Equal(t, "https://example.com/repo.git", rc.URLs[0])
}

func TestResolveAgentUpstream_PrefersAgentRefWhenPresent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Manually create a remote-tracking ref for the agent.
	agentHash := mustHeadHash(t, svc, "agent/test")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(
			plumbing.NewRemoteReferenceName("origin", "agent/test"),
			agentHash,
		),
	))
	// And one for main.
	mainHash := mustHeadHash(t, svc, "main")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(
			plumbing.NewRemoteReferenceName("origin", "main"),
			mainHash,
		),
	))

	got, err := svc.rh.resolveAgentUpstream(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, "refs/remotes/origin/agent/test", got.refName.String())
	require.Equal(t, agentHash, got.hash)
	require.True(t, got.isOwnAgent, "agent upstream must flag isOwnAgent")
}

func TestResolveAgentUpstream_FallsBackToMain(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Only origin/main is present; no origin/agent/test.
	mainHash := mustHeadHash(t, svc, "main")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(
			plumbing.NewRemoteReferenceName("origin", "main"),
			mainHash,
		),
	))

	got, err := svc.rh.resolveAgentUpstream(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, "refs/remotes/origin/main", got.refName.String())
	require.Equal(t, mainHash, got.hash)
	require.False(t, got.isOwnAgent, "main-fallback upstream must NOT flag isOwnAgent")
}

func TestResolveAgentUpstream_NoUpstreamIsError(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.rh.resolveAgentUpstream(context.Background(), "agent/test")
	require.Error(t, err, "no origin/main and no origin/agent ref → must error")
}

func mustHeadHash(t *testing.T, svc *Service, branch string) plumbing.Hash {
	t.Helper()
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	return ref.Hash()
}

func TestUnpushedCommits_LocalAhead(t *testing.T) {
	// Setup: init repo (one root commit on agent + main), write two more facts on agent.
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	rootHash := mustHeadHash(t, svc, "main") // same as agent root at this point
	c1 := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	c2 := writeMergeFact(t, svc, "agent/test", "kb/b.md", "B", "v1")

	commits, disjoint, err := svc.rh.unpushedCommits(plumbing.NewHash(c2), rootHash)
	require.NoError(t, err)
	require.False(t, disjoint)
	require.Len(t, commits, 2)
	require.Equal(t, c1, commits[0].Hash.String())
	require.Equal(t, c2, commits[1].Hash.String())
}

func TestUnpushedCommits_AlreadyUpstreamAncestor(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Local agent === root; if we treat root as upstream, nothing unpushed.
	rootHash := mustHeadHash(t, svc, "agent/test")
	commits, disjoint, err := svc.rh.unpushedCommits(rootHash, rootHash)
	require.NoError(t, err)
	require.False(t, disjoint)
	require.Empty(t, commits)
}

func TestUnpushedCommits_DisjointReturnsAllLocal(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	c1 := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")

	// Create a totally unrelated root commit in the same object store.
	disjointHash := makeDisjointRoot(t, svc, "disjoint/root.md", "unrelated content")

	commits, disjoint, err := svc.rh.unpushedCommits(plumbing.NewHash(c1), disjointHash)
	require.NoError(t, err)
	require.True(t, disjoint, "must report disjoint when no merge base exists")
	require.NotEmpty(t, commits, "all local commits must be returned for disjoint replay")
	// First commit oldest, last newest:
	require.Equal(t, c1, commits[len(commits)-1].Hash.String())
}

// makeDisjointRoot writes a single commit with no parent into the storer
// and returns its hash. The commit has nothing in common with InitRepo's
// root, so MergeBase against it returns empty.
func makeDisjointRoot(t *testing.T, svc *Service, path, content string) plumbing.Hash {
	t.Helper()
	hash, _, err := writeFileToStore(
		svc.rh.gits, plumbing.ZeroHash, path, content,
		"disjoint root",
		object.Signature{Name: "test", Email: "t@t"},
		object.Signature{Name: "test", Email: "t@t"},
	)
	require.NoError(t, err)
	return hash
}
