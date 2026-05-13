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

func TestReplayCommit_PreservesAuthorAndMessage(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	c1Hash := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	c1, err := svc.rh.repo.CommitObject(plumbing.NewHash(c1Hash))
	require.NoError(t, err)

	// Create a "new base" — an unrelated commit on main with a different file.
	c2Hash := writeMergeFact(t, svc, "main", "kb/other.md", "Other", "ov1")
	c2, err := svc.rh.repo.CommitObject(plumbing.NewHash(c2Hash))
	require.NoError(t, err)

	newHash, err := svc.rh.replayCommit(context.Background(), c1, c2.Hash, StrategyLocalWins)
	require.NoError(t, err)

	newCommit, err := svc.rh.repo.CommitObject(newHash)
	require.NoError(t, err)

	require.Equal(t, c1.Author.Name, newCommit.Author.Name, "author preserved")
	require.Equal(t, c1.Author.Email, newCommit.Author.Email, "author email preserved")
	require.Equal(t, c1.Message, newCommit.Message, "message preserved")
	require.Equal(t, 1, newCommit.NumParents(), "replayed commit has single parent")
	require.Equal(t, c2.Hash, newCommit.ParentHashes[0], "parent is the new base")

	// Tree must contain both kb/a.md (from c1) and kb/other.md (from c2).
	tree, err := newCommit.Tree()
	require.NoError(t, err)
	_, err = tree.File("kb/a.md")
	require.NoError(t, err, "replayed file present")
	_, err = tree.File("kb/other.md")
	require.NoError(t, err, "base file preserved")
}

// TestReplayCommit_LocalWinsOnConflict drives a true Modify-vs-Modify
// three-way merge inside replayCommit. The seed (kb/shared.md = "base-version")
// is shared between agent/test and main so the diff (base → c1) registers as
// Modify, and dst (main) has independently modified the same path — the only
// shape that exercises the strategy branch in mergeTreesWithStrategy.
//
// In the replay caller's framing, StrategyLocalWins means "agent wins": c1's
// content ("agent-version") must survive on the replayed commit.
func TestReplayCommit_LocalWinsOnConflict(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// 1. Seed kb/shared.md on agent/test. This commit becomes c1's parent
	//    AND will be where main starts — making it the merge base.
	baseHash := writeMergeFact(t, svc, "agent/test", "kb/shared.md", "Shared", "base-version")

	// 2. Reset main to that commit so both branches share the seed.
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(baseHash)),
	))

	// 3. Agent modifies kb/shared.md (Modify on agent side).
	c1Hash := writeMergeFact(t, svc, "agent/test", "kb/shared.md", "Shared", "agent-version")
	c1, err := svc.rh.repo.CommitObject(plumbing.NewHash(c1Hash))
	require.NoError(t, err)
	// Sanity-check: c1's parent is the seed commit.
	require.Equal(t, plumbing.NewHash(baseHash), c1.ParentHashes[0], "c1 must parent the seed")

	// 4. Main also modifies the same file (Modify on upstream side).
	mainHash := writeMergeFact(t, svc, "main", "kb/shared.md", "Shared", "main-version")

	// 5. Replay with LocalWins (agent's perspective: agent should win).
	newHash, err := svc.rh.replayCommit(context.Background(), c1, plumbing.NewHash(mainHash), StrategyLocalWins)
	require.NoError(t, err)
	newCommit, err := svc.rh.repo.CommitObject(newHash)
	require.NoError(t, err)

	tree, err := newCommit.Tree()
	require.NoError(t, err)
	f, err := tree.File("kb/shared.md")
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)
	require.Contains(t, content, "agent-version", "LocalWins on Modify-vs-Modify: agent content must survive")
}

// TestReplayCommit_RemoteWinsOnConflict is the inverse of the LocalWins test.
// Same Modify-vs-Modify setup, but with StrategyRemoteWins — the upstream
// (main) content must survive.
func TestReplayCommit_RemoteWinsOnConflict(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	baseHash := writeMergeFact(t, svc, "agent/test", "kb/shared.md", "Shared", "base-version")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(baseHash)),
	))
	c1Hash := writeMergeFact(t, svc, "agent/test", "kb/shared.md", "Shared", "agent-version")
	c1, err := svc.rh.repo.CommitObject(plumbing.NewHash(c1Hash))
	require.NoError(t, err)
	mainHash := writeMergeFact(t, svc, "main", "kb/shared.md", "Shared", "main-version")

	newHash, err := svc.rh.replayCommit(context.Background(), c1, plumbing.NewHash(mainHash), StrategyRemoteWins)
	require.NoError(t, err)
	newCommit, err := svc.rh.repo.CommitObject(newHash)
	require.NoError(t, err)
	tree, err := newCommit.Tree()
	require.NoError(t, err)
	f, err := tree.File("kb/shared.md")
	require.NoError(t, err)
	content, err := f.Contents()
	require.NoError(t, err)
	require.Contains(t, content, "main-version", "RemoteWins on Modify-vs-Modify: upstream content must survive")
}

func TestReplayOntoUpstream_NoUnpushedIsNoOp(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	agentHash := mustHeadHash(t, svc, "agent/test")
	// Upstream = same hash → unpushed commits is empty → no-op.
	result, err := svc.rh.replayOntoUpstream(
		context.Background(), "agent/test", agentHash, StrategyLocalWins,
	)
	require.NoError(t, err)
	require.False(t, result.Replayed, "no-op must not report Replayed")
	require.Equal(t, agentHash, mustHeadHash(t, svc, "agent/test"), "ref unchanged")
}

func TestReplayOntoUpstream_FastForwardWhenLocalIsAncestor(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Upstream gets a commit; agent stays behind.
	upstreamHash := writeMergeFact(t, svc, "main", "kb/u.md", "U", "v1")
	result, err := svc.rh.replayOntoUpstream(
		context.Background(), "agent/test", plumbing.NewHash(upstreamHash), StrategyLocalWins,
	)
	require.NoError(t, err)
	require.True(t, result.FastForward, "must fast-forward when local is ancestor")
	require.Equal(t, plumbing.NewHash(upstreamHash), mustHeadHash(t, svc, "agent/test"))
}

func TestReplayOntoUpstream_ReplaysAllUnpushedCommits(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Two local commits.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	writeMergeFact(t, svc, "agent/test", "kb/b.md", "B", "v1")
	// One commit on main = upstream.
	upstreamHash := writeMergeFact(t, svc, "main", "kb/u.md", "U", "uv1")

	result, err := svc.rh.replayOntoUpstream(
		context.Background(), "agent/test", plumbing.NewHash(upstreamHash), StrategyLocalWins,
	)
	require.NoError(t, err)
	require.True(t, result.Replayed)
	require.Equal(t, 2, result.NumReplayed, "two commits replayed")

	// New agent tip is a linear chain ending with replayed commits.
	newTip := mustHeadHash(t, svc, "agent/test")
	require.NotEqual(t, plumbing.NewHash(upstreamHash), newTip)

	tip, err := svc.rh.repo.CommitObject(newTip)
	require.NoError(t, err)
	require.Equal(t, 1, tip.NumParents(), "linear")
	// Walk back: tip → mid → upstream.
	mid, err := tip.Parents().Next()
	require.NoError(t, err)
	require.Equal(t, 1, mid.NumParents(), "linear")
	require.Equal(t, plumbing.NewHash(upstreamHash), mid.ParentHashes[0], "chain rooted at upstream")

	// All three files present in final tree.
	tree, err := tip.Tree()
	require.NoError(t, err)
	_, err = tree.File("kb/a.md")
	require.NoError(t, err)
	_, err = tree.File("kb/b.md")
	require.NoError(t, err)
	_, err = tree.File("kb/u.md")
	require.NoError(t, err)
}

func TestReplayOntoUpstream_FailureLeavesAgentRefUntouched(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	preReplayHash := mustHeadHash(t, svc, "agent/test")

	// Inject failure by passing an unreadable upstream hash.
	bogus := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	_, err = svc.rh.replayOntoUpstream(
		context.Background(), "agent/test", bogus, StrategyLocalWins,
	)
	require.Error(t, err, "must error on bad upstream")
	require.Equal(t, preReplayHash, mustHeadHash(t, svc, "agent/test"), "agent ref unchanged on failure")
}

func TestReconcileMain_FastForwardsWhenOriginAhead(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// origin/main is a descendant of local main (new commit on it).
	newMainCommit := writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")
	// Move main back to its parent to simulate "we're behind origin/main".
	parent, err := svc.rh.repo.CommitObject(plumbing.NewHash(newMainCommit))
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), parent.ParentHashes[0]),
	))
	// origin/main points at the newer commit.
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(newMainCommit)),
	))

	res, err := svc.rh.reconcileMain(context.Background())
	require.NoError(t, err)
	require.True(t, res.FastForward)
	require.False(t, res.Rewound)
	require.Equal(t, plumbing.NewHash(newMainCommit), mustHeadHash(t, svc, "main"))
}

func TestReconcileMain_NoOpWhenAlreadyAtOrigin(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	mainHash := mustHeadHash(t, svc, "main")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), mainHash),
	))

	res, err := svc.rh.reconcileMain(context.Background())
	require.NoError(t, err)
	require.False(t, res.FastForward)
	require.False(t, res.Rewound)
}

func TestReconcileMain_DetectsRewind(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Local main has a commit; origin/main is a disjoint commit.
	writeMergeFact(t, svc, "main", "kb/local.md", "L", "v1")
	disjointHash := makeDisjointRoot(t, svc, "kb/origin.md", "origin content")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), disjointHash),
	))

	res, err := svc.rh.reconcileMain(context.Background())
	require.NoError(t, err)
	require.True(t, res.Rewound, "non-descendant origin/main must report Rewound")
	require.Equal(t, disjointHash, mustHeadHash(t, svc, "main"), "local main force-updated to origin")
}

func TestReconcileMain_NoOriginMainIsError(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.rh.reconcileMain(context.Background())
	require.Error(t, err)
}

func TestReconcileMain_CreatesLocalMainWhenMissing(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Drop local main so reconcileMain has to create it.
	require.NoError(t, svc.rh.gits.RemoveReference(plumbing.NewBranchReferenceName("main")))

	// Set origin/main to some content.
	originHash := writeMergeFact(t, svc, "agent/test", "kb/o.md", "O", "v1")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(originHash)),
	))

	res, err := svc.rh.reconcileMain(context.Background())
	require.NoError(t, err)
	require.True(t, res.FastForward, "creating missing main is reported as fast-forward")
	require.Equal(t, plumbing.NewHash(originHash), mustHeadHash(t, svc, "main"), "local main now at origin")
}

func TestReconcileAgent_UsesAgentUpstreamWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Make origin/agent/test ahead of local agent.
	upstreamHash := writeMergeFact(t, svc, "agent/test", "kb/u.md", "U", "v1")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "agent/test"), plumbing.NewHash(upstreamHash)),
	))
	// Move local agent back so it's behind.
	upstreamCommit, err := svc.rh.repo.CommitObject(plumbing.NewHash(upstreamHash))
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("agent/test"), upstreamCommit.ParentHashes[0]),
	))
	// origin/main also present but should be IGNORED (agent upstream is preferred).
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), upstreamCommit.ParentHashes[0]),
	))

	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", StrategyLocalWins)
	require.NoError(t, err)
	require.True(t, res.FastForward, "agent fast-forwards to origin/agent")
	require.Equal(t, plumbing.NewHash(upstreamHash), mustHeadHash(t, svc, "agent/test"))
}

func TestReconcileAgent_FallsBackToMainWhenNoAgentUpstream(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// One local commit on agent, one on main; only origin/main is present.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	mainHash := writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(mainHash)),
	))

	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", StrategyLocalWins)
	require.NoError(t, err)
	require.True(t, res.Replayed)
	require.Equal(t, 1, res.NumReplayed)

	// Tree at new tip has both files.
	newTip := mustHeadHash(t, svc, "agent/test")
	commit, err := svc.rh.repo.CommitObject(newTip)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.File("kb/a.md")
	require.NoError(t, err)
	_, err = tree.File("kb/m.md")
	require.NoError(t, err)
}

func TestSync_OrchestratesMainAndAgent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Pretend a fetch happened: write origin/main ref directly.
	originMain := writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(originMain)),
	))
	// Move local main back to its parent so reconcileMain has work to do.
	originMainCommit, err := svc.rh.repo.CommitObject(plumbing.NewHash(originMain))
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), originMainCommit.ParentHashes[0]),
	))
	// One unpushed commit on agent.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")

	// Use the in-process reconcile entry point that skips fetch (no real remote).
	res, err := svc.Remote().(*remoteIndex).reconcileNow(context.Background(), "agent/test")
	require.NoError(t, err)
	require.True(t, res.Agent.Replayed, "agent must replay")
	require.Equal(t, plumbing.NewHash(originMain), mustHeadHash(t, svc, "main"))
}

func TestPush_ForcePushesAgent(t *testing.T) {
	t.Log("agent branch is force-pushed; reconcile-before-push handles upstream drift")
	// Full coverage lives in storytests/reconcile_test.go (Task 15) where the
	// testenv BareRemote DSL is available. This package-level test is a
	// placeholder marking intent; storytests exercise the real bare-repo
	// round-trip.
	t.Skip("covered by storytests/reconcile_test.go (Task 15)")
}
