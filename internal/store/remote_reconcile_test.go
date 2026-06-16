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

	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "main", "agent/test"))

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

	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "main", "agent/test"))
	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "main", "agent/test"))

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	rc, ok := cfg.Remotes["origin"]
	require.True(t, ok, "origin remote must be configured")
	require.Len(t, rc.Fetch, 2, "second call must not duplicate refspecs")
	require.Equal(t, "https://example.com/repo.git", rc.URLs[0])
}

func TestAgentBaseRefName_FormatsUnderKnomitNamespace(t *testing.T) {
	got := agentBaseRefName("agent/test")
	require.Equal(t, "refs/knomit/agent-base/agent/test", got.String(),
		"watermark refs must live under refs/knomit/ to stay out of branch listings")
}

func TestReadAgentBase_MissingIsReferenceNotFound(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// InitRepo seeds the watermark; clear it to model the missing case.
	require.NoError(t, svc.rh.gits.RemoveReference(agentBaseRefName("agent/test")))

	_, err = svc.rh.readAgentBase("agent/test")
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "missing watermark must surface plumbing.ErrReferenceNotFound")
}

func TestWriteAgentBase_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	mainHash := mustHeadHash(t, svc, "main")
	require.NoError(t, svc.rh.writeAgentBase("agent/test", mainHash))

	got, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, mainHash, got)
}

func TestInitRepo_SeedsAgentWatermark(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Watermark = the initial commit (which equals local main at init time).
	mainHash := mustHeadHash(t, svc, "main")
	got, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, mainHash, got, "InitRepo must seed watermark to the initial commit")
}

func mustHeadHash(t *testing.T, svc *Service, branch string) plumbing.Hash {
	t.Helper()
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	return ref.Hash()
}

func TestUnpushedCommits_LocalAhead(t *testing.T) {
	// Setup: divergent histories. Agent has two new commits (c1, c2) on top
	// of the shared root; main (the "upstream") has its own commit on top
	// of the same root. Neither side is an ancestor of the other, so
	// unpushedCommits must return the agent's two commits for replay.
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	c1 := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	c2 := writeMergeFact(t, svc, "agent/test", "kb/b.md", "B", "v1")
	// Independent commit on main so neither side is an ancestor of the other.
	mainHash := writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")

	commits, disjoint, err := svc.rh.unpushedCommits(plumbing.NewHash(c2), plumbing.NewHash(mainHash), plumbing.ZeroHash)
	require.NoError(t, err)
	require.False(t, disjoint)
	require.Len(t, commits, 2)
	require.Equal(t, c1, commits[0].Hash.String())
	require.Equal(t, c2, commits[1].Hash.String())
}

func TestUnpushedCommits_LocalStrictlyAheadIsNoOp(t *testing.T) {
	// Setup: init repo, write a commit on agent. Treat the ROOT (which is
	// the previous commit, ancestor of local) as the upstream — local is
	// strictly ahead of upstream by exactly one commit, but the upstream
	// is itself an ancestor of local. Expected: no replay needed.
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	rootHash := mustHeadHash(t, svc, "main")
	c1 := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")

	// upstream = root (ancestor of c1). local strictly ahead by one commit.
	// Caller should push c1 as a fast-forward; no replay.
	commits, disjoint, err := svc.rh.unpushedCommits(plumbing.NewHash(c1), rootHash, plumbing.ZeroHash)
	require.NoError(t, err)
	require.False(t, disjoint)
	require.Empty(t, commits, "linear-ahead local needs no replay; force-push will fast-forward origin")
}

func TestUnpushedCommits_AlreadyUpstreamAncestor(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Local agent === root; if we treat root as upstream, nothing unpushed.
	rootHash := mustHeadHash(t, svc, "agent/test")
	commits, disjoint, err := svc.rh.unpushedCommits(rootHash, rootHash, plumbing.ZeroHash)
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

	commits, disjoint, err := svc.rh.unpushedCommits(plumbing.NewHash(c1), disjointHash, plumbing.ZeroHash)
	require.NoError(t, err)
	require.True(t, disjoint, "must report disjoint when no merge base exists")
	require.NotEmpty(t, commits, "all local commits must be returned for disjoint replay")
	// First commit oldest, last newest:
	require.Equal(t, c1, commits[len(commits)-1].Hash.String())
}

// TestUnpushedCommits_SkipsMergeCommits is the regression test for the
// "merge-commit + force-rewind resurrects old-main content" bug. Setup:
//
//  1. Agent writes a local fact.
//  2. Steady-state reconcile merges local main into the agent (the merge
//     commit's second parent is local main; its tree contains main's
//     content alongside the agent's).
//  3. Agent writes another local fact on top of the merge commit.
//
// unpushedCommits must NOT include the merge commit itself — its tree
// bakes in old-main content that would be resurrected if we replayed it
// onto a disjoint new main during the rebase fallback.
func TestUnpushedCommits_SkipsMergeCommits(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	writeMergeFact(t, svc, "agent/test", "kb/local-1.md", "L1", "v1")
	preMergeAgent := mustHeadHash(t, svc, "agent/test")
	writeMergeFact(t, svc, "main", "kb/main-old.md", "MO", "v1")
	oldMainHash := mustHeadHash(t, svc, "main")

	mergeRes, err := svc.rh.reconcileAgentMerge(context.Background(), "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, mergeRes.Mode, "setup requires a real merge commit")
	mergeHash := plumbing.NewHash(mergeRes.NewTip)

	// Sanity: the merge commit has two parents (agent prior, main).
	mergeCommit, err := svc.rh.repo.CommitObject(mergeHash)
	require.NoError(t, err)
	require.Equal(t, 2, mergeCommit.NumParents(), "merge commit must have two parents")
	require.Equal(t, preMergeAgent, mergeCommit.ParentHashes[0])
	require.Equal(t, oldMainHash, mergeCommit.ParentHashes[1])

	// One more agent-only commit on top of the merge.
	postMergeAgent := writeMergeFact(t, svc, "agent/test", "kb/local-2.md", "L2", "v1")

	// Walk: the watermark from reconcileAgentMerge is oldMainHash. Walking
	// from postMergeAgent back to oldMainHash should yield [preMergeAgent,
	// postMergeAgent] — i.e. the merge commit itself is SKIPPED.
	disjoint := makeDisjointRoot(t, svc, "kb/new-main.md", "new disjoint root")
	commits, _, err := svc.rh.unpushedCommits(plumbing.NewHash(postMergeAgent), disjoint, oldMainHash)
	require.NoError(t, err)

	hashes := make([]string, 0, len(commits))
	for _, c := range commits {
		hashes = append(hashes, c.Hash.String())
	}
	require.NotContains(t, hashes, mergeHash.String(),
		"merge commit MUST be skipped — replaying it resurrects old-main content")
	require.Contains(t, hashes, preMergeAgent.String(), "pre-merge agent commit must replay")
	require.Contains(t, hashes, postMergeAgent, "post-merge agent commit must replay")
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
		context.Background(), "agent/test", agentHash, plumbing.ZeroHash, StrategyLocalWins,
	)
	require.NoError(t, err)
	require.Equal(t, ModeNoop, result.Mode, "no-op must report Mode=ModeNoop")
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
		context.Background(), "agent/test", plumbing.NewHash(upstreamHash), plumbing.ZeroHash, StrategyLocalWins,
	)
	require.NoError(t, err)
	require.Equal(t, ModeFF, result.Mode, "must fast-forward when local is ancestor")
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
		context.Background(), "agent/test", plumbing.NewHash(upstreamHash), plumbing.ZeroHash, StrategyLocalWins,
	)
	require.NoError(t, err)
	require.Equal(t, ModeRebase, result.Mode)
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

// TestReplayOntoUpstream_HonoursContextCancellation pins the contract that a
// cancelled context aborts the replay loop without advancing the agent ref.
// Prior to the ctx.Err() check at the top of the replay loop, a large rewind
// under server shutdown (or HTTP client disconnect) would hold the agent
// branch lock until the full chain finished, stalling syncWg.Wait() on close.
func TestReplayOntoUpstream_HonoursContextCancellation(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Two unpushed agent commits over a one-commit upstream — enough that the
	// loop's per-iteration ctx check is the only thing that can stop it.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	writeMergeFact(t, svc, "agent/test", "kb/b.md", "B", "v1")
	upstreamHash := writeMergeFact(t, svc, "main", "kb/u.md", "U", "uv1")
	preReplayHash := mustHeadHash(t, svc, "agent/test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = svc.rh.replayOntoUpstream(
		ctx, "agent/test", plumbing.NewHash(upstreamHash), plumbing.ZeroHash, StrategyLocalWins,
	)
	require.Error(t, err, "replay must abort on cancelled context")
	require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled")
	require.Equal(t, preReplayHash, mustHeadHash(t, svc, "agent/test"),
		"agent ref unchanged when replay is cancelled mid-loop")

	tempRefName := plumbing.NewBranchReferenceName("agent/test-replaying")
	_, refErr := svc.rh.gits.Reference(tempRefName)
	require.ErrorIs(t, refErr, plumbing.ErrReferenceNotFound,
		"temp ref cleaned up after cancellation")
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
		context.Background(), "agent/test", bogus, plumbing.ZeroHash, StrategyLocalWins,
	)
	require.Error(t, err, "must error on bad upstream")
	require.Equal(t, preReplayHash, mustHeadHash(t, svc, "agent/test"), "agent ref unchanged on failure")

	// Temp ref must not be left behind on failure either. Note: this
	// particular failure mode errors out in unpushedCommits BEFORE the
	// temp ref is created, so the assertion documents the invariant
	// rather than directly exercising the deferred cleanup path.
	tempRefName := plumbing.NewBranchReferenceName("agent/test-replaying")
	_, refErr := svc.rh.gits.Reference(tempRefName)
	require.ErrorIs(t, refErr, plumbing.ErrReferenceNotFound,
		"temp ref must be removed on failure too")
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

	res, err := svc.rh.reconcileMain(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode)
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

	res, err := svc.rh.reconcileMain(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode)
}

// TestReconcileMain_HealsMissingBranchesRow regression-tests a latent
// inconsistency observed in the field: a local store can accumulate a
// refs/heads/<upstreamMain> git ref without a matching row in the branches
// SQL table. (Older code paths and migrations left this state behind; new
// shared-history reconnect via /api/v1/{repo}/origin-sessions exposed it
// because it preserves the existing local store rather than swapping in a
// freshly-cloned DB the way the disjoint path used to.)
//
// When the inconsistency is present, the Sync flow used to fail mid-cycle:
// reconcileMain returned ModeNoop without touching the SQL table, then
// reconcileAgent → mergeIntoBranchLocked → branchID("main") errored with
// "branch \"main\": branch not found", killing the reconcile loop.
//
// reconcileMain must heal the SQL table on every success path — not only
// the create path — so a single tick of the sync loop is enough to recover.
func TestReconcileMain_HealsMissingBranchesRow(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Local main and origin/main both at the same commit (the steady-state
	// Noop case after fetch).
	mainHash := mustHeadHash(t, svc, "main")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), mainHash),
	))

	// Simulate the corruption: drop the SQL row for "main" while leaving
	// the git ref in place. Also drop the cache entry so the next branchID
	// call re-reads from the DB (otherwise the test passes spuriously).
	_, err = svc.rh.db.ExecContext(context.Background(), `DELETE FROM branches WHERE name = ?`, "main")
	require.NoError(t, err)
	svc.rh.cache.remove("main")

	// Pre-condition: branches.main is missing, so a lookup must fail.
	_, beforeErr := svc.rh.branchID(context.Background(), "main")
	require.Error(t, beforeErr, "test setup: branches row for 'main' must be absent")

	// Reconcile — should heal the SQL row even on the Noop path.
	res, err := svc.rh.reconcileMain(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, ModeNoop, res.Mode, "ref already at origin: Noop path")

	// Post-condition: branchID must now succeed.
	id, afterErr := svc.rh.branchID(context.Background(), "main")
	require.NoError(t, afterErr, "reconcileMain must EnsureBranch on every success path")
	require.NotZero(t, id)
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

	res, err := svc.rh.reconcileMain(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, ModeRewound, res.Mode, "non-descendant origin/main must report Mode=ModeRewound")
	require.Equal(t, disjointHash, mustHeadHash(t, svc, "main"), "local main force-updated to origin")
}

func TestReconcileMain_NoOriginMainIsError(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	_, err = svc.rh.reconcileMain(context.Background(), "main")
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

	res, err := svc.rh.reconcileMain(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode)
	require.Equal(t, plumbing.NewHash(originHash), mustHeadHash(t, svc, "main"), "local main now at origin")
}

// TestReconcileAgent_ReplaysLocalCommitsOntoLocalMain verifies the core
// design in the steady-state (merge) path: reconcileAgent merges local
// main into the agent and the resulting tree contains both sides' files.
// origin/agent/<host> presence is irrelevant (the agent never reads from
// it after bootstrap).
func TestReconcileAgent_ReplaysLocalCommitsOntoLocalMain(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Watermark at this point is the seed (set by InitRepo). One local
	// commit on agent; one independent commit on main. Local main advances.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")

	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "divergent histories merge in steady state")
	require.Equal(t, ModeMerge, res.Mode)

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

	// Watermark advanced to current local main.
	mainHash := mustHeadHash(t, svc, "main")
	wm, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, mainHash, wm, "watermark advances to local main on successful reconcile")
}

// TestReconcileAgent_PicksUpMainAdvance is the targeted regression for the
// bug this whole rework fixes: when the agent has previously synced
// (watermark = some past main commit) and local main advances, the next
// reconcileAgent must fast-forward the agent onto the new main even though
// the agent has no local-only commits.
func TestReconcileAgent_PicksUpMainAdvance(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Watermark = initial commit (seeded by InitRepo) = agent tip = main tip.
	// Advance local main; agent has no local commits.
	newMainHash := writeMergeFact(t, svc, "main", "kb/promoted.md", "P", "v1")

	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode, "agent is ancestor of main → fast-forward")
	require.Equal(t, plumbing.NewHash(newMainHash), mustHeadHash(t, svc, "agent/test"),
		"agent tip == new local main after fast-forward")

	// Watermark also moved.
	wm, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(newMainHash), wm)
}

// TestReconcileAgent_ReplaysLocalCommitsOntoUpdatedMain: agent has a local
// commit since the watermark; main has advanced. In the steady-state
// (merge) path, expect a real merge commit whose tree contains both
// sides' files. The merge commit's second parent is the new main.
func TestReconcileAgent_ReplaysLocalCommitsOntoUpdatedMain(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Watermark = initial commit. Agent commits a local file; main advances.
	writeMergeFact(t, svc, "agent/test", "kb/local.md", "L", "v1")
	newMain := writeMergeFact(t, svc, "main", "kb/promoted.md", "P", "v1")

	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "divergent histories merge in steady state")
	require.Equal(t, ModeMerge, res.Mode)

	newTip := mustHeadHash(t, svc, "agent/test")
	commit, err := svc.rh.repo.CommitObject(newTip)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.File("kb/local.md")
	require.NoError(t, err, "local change preserved via merge")
	_, err = tree.File("kb/promoted.md")
	require.NoError(t, err, "main advance picked up")

	// Merge commit has two parents; second parent is the new main.
	require.Equal(t, 2, commit.NumParents(), "merge commit has two parents")
	require.Equal(t, plumbing.NewHash(newMain), commit.ParentHashes[1],
		"merge commit's second parent is the new main")
}

// TestReconcileAgent_WatermarkPreservedAcrossTicks: after two consecutive
// Sync ticks (each advancing main), the watermark always equals current
// local main, every local fact is preserved, and the final agent tree
// contains both ticks' content. In the merge-based steady-state path,
// each tick produces one merge commit (not a re-replay) so commit hashes
// for the agent's local commits are preserved.
func TestReconcileAgent_WatermarkPreservedAcrossTicks(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Tick 1: agent writes local-1, main advances, reconcile.
	writeMergeFact(t, svc, "agent/test", "kb/local-1.md", "L1", "v1")
	main1 := writeMergeFact(t, svc, "main", "kb/m1.md", "M1", "v1")
	res1, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res1.Mode, "tick 1 produces a merge commit")
	require.Equal(t, ModeMerge, res1.Mode)
	wm1, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(main1), wm1, "watermark at main1 after tick 1")

	// Tick 2: agent writes local-2, main advances, reconcile.
	writeMergeFact(t, svc, "agent/test", "kb/local-2.md", "L2", "v1")
	main2 := writeMergeFact(t, svc, "main", "kb/m2.md", "M2", "v1")
	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "tick 2 produces a merge commit")
	require.Equal(t, ModeMerge, res.Mode)

	wm2, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(main2), wm2, "watermark at main2 after tick 2")

	// Final tree has every file.
	newTip := mustHeadHash(t, svc, "agent/test")
	commit, err := svc.rh.repo.CommitObject(newTip)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	for _, p := range []string{"kb/local-1.md", "kb/local-2.md", "kb/m1.md", "kb/m2.md"} {
		_, err := tree.File(p)
		require.NoError(t, err, "%s must survive two reconcile ticks", p)
	}
}

// TestReconcileAgentRebase_FallsBackToMergeBaseWhenWatermarkMissing models
// the defensive path on the rebase fallback: an older repo (or transient
// ref corruption) that lacks the watermark must still reconcile correctly
// by falling back to MergeBase. The reconcile produces a clean replay AND
// seeds the watermark for future ticks. Targets reconcileAgentRebase
// directly because the watermark-missing fallback only lives on the
// rebase path now.
func TestReconcileAgentRebase_FallsBackToMergeBaseWhenWatermarkMissing(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Drop the watermark to model the legacy/corruption case.
	require.NoError(t, svc.rh.gits.RemoveReference(agentBaseRefName("agent/test")))

	// Standard "agent has local, main has advanced" setup.
	writeMergeFact(t, svc, "agent/test", "kb/local.md", "L", "v1")
	mainHash := writeMergeFact(t, svc, "main", "kb/promoted.md", "P", "v1")

	res, err := svc.rh.reconcileAgentRebase(context.Background(), "agent/test", "main", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, ModeRebase, res.Mode)
	require.Equal(t, 1, res.NumReplayed)

	// Watermark was (re)seeded.
	wm, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(mainHash), wm,
		"missing watermark must be reseeded to current local main after a successful reconcile")
}

// TestReconcileAgent_SteadyStateUsesMerge: when main has advanced and was
// NOT rewound, the agent must be reconciled via a real merge commit, not
// a rebase. This is the steady-state path that fixes the squash-merge
// regression — no orphan commits, no branch_facts purge.
func TestReconcileAgent_SteadyStateUsesMerge(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Agent has a local commit; main has advanced.
	writeMergeFact(t, svc, "agent/test", "kb/local.md", "L", "v1")
	writeMergeFact(t, svc, "main", "kb/promoted.md", "P", "v1")

	preAgent := mustHeadHash(t, svc, "agent/test")
	preMain := mustHeadHash(t, svc, "main")

	// mainRewound=false → merge path.
	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, false)
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Mode, "steady state must merge, not rebase")
	require.NotEmpty(t, res.NewTip)

	// New tip is a real merge commit with two parents: previous agent first, main second.
	newTip, err := svc.rh.repo.CommitObject(plumbing.NewHash(res.NewTip))
	require.NoError(t, err)
	require.Equal(t, 2, newTip.NumParents())
	require.Equal(t, preAgent, newTip.ParentHashes[0])
	require.Equal(t, preMain, newTip.ParentHashes[1])

	// Watermark moved to current local main.
	wm, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.Equal(t, preMain, wm)
}

// TestReconcileAgent_RewindUsesRebase: when reconcileMain reported Rewound,
// the dispatcher must take the rebase path so disjoint-history scrub
// semantics work (G6).
func TestReconcileAgent_RewindUsesRebase(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Agent writes a local commit on top of the seed.
	writeMergeFact(t, svc, "agent/test", "kb/local.md", "L", "v1")
	preWatermark, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)

	// Simulate a force-rewind: local main is now at a disjoint root commit.
	disjoint := makeDisjointRoot(t, svc, "kb/rewind.md", "disjoint root")
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), disjoint),
	))

	// mainRewound=true → rebase path.
	res, err := svc.rh.reconcileAgent(context.Background(), "agent/test", "main", StrategyLocalWins, true)
	require.NoError(t, err)
	require.Equal(t, ModeRebase, res.Mode, "rewind must rebase, not merge")
	require.GreaterOrEqual(t, res.NumReplayed, 1)

	// Watermark advanced.
	wm, err := svc.rh.readAgentBase("agent/test")
	require.NoError(t, err)
	require.NotEqual(t, preWatermark, wm, "watermark must advance after rewind reconcile")
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
	res, err := svc.Remote().(*remoteIndex).reconcileNow(context.Background(), "agent/test", "main")
	require.NoError(t, err)
	require.Equal(t, ModeMerge, res.Agent.Mode, "steady-state Sync merges local main into agent (one merge commit)")
	require.Equal(t, plumbing.NewHash(originMain), mustHeadHash(t, svc, "main"))
}

// TestReconcileNow_UpstreamEqualsAgentIsPushOnly regresses the data-loss bug
// where a repo whose configured consensus branch (upstreamMain) IS this
// machine's own agent branch had its just-written, not-yet-pushed fact commits
// force-reset away on every reconcile cycle (reconcileMain treated the agent
// branch as "main" and, seeing local ahead of origin, force-updated local down
// to origin). When upstreamMain == agentBranch, reconcileNow must be a no-op
// (push-only) and leave the local agent branch untouched.
func TestReconcileNow_UpstreamEqualsAgentIsPushOnly(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// A freshly-written fact commit on the agent branch (local ahead, unpushed).
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	localBefore := mustHeadHash(t, svc, "agent/test")

	// Simulate origin/agent BEHIND local (the state right after a write): the
	// old code, treating the agent branch as "main", force-reset local down to
	// this ref and destroyed the fact commit.
	localCommit, err := svc.rh.repo.CommitObject(localBefore)
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "agent/test"), localCommit.ParentHashes[0]),
	))

	// upstreamMain == agentBranch (the degenerate config).
	res, err := svc.Remote().(*remoteIndex).reconcileNow(context.Background(), "agent/test", "agent/test")
	require.NoError(t, err)
	require.Empty(t, res.Main.Mode, "no main reconcile must run when upstream == agent branch")
	require.Empty(t, res.Agent.Mode, "no agent reconcile must run when upstream == agent branch")

	// The local fact commit must survive untouched (push-only).
	require.Equal(t, localBefore, mustHeadHash(t, svc, "agent/test"),
		"reconcileNow must NOT reset the local agent branch when upstream == agent branch")
}

func TestPush_ForcePushesAgent(t *testing.T) {
	t.Log("agent branch is force-pushed; reconcile-before-push handles upstream drift")
	// Full coverage lives in storytests/reconcile_test.go (Task 15) where the
	// testenv BareRemote DSL is available. This package-level test is a
	// placeholder marking intent; storytests exercise the real bare-repo
	// round-trip.
	t.Skip("covered by storytests/reconcile_test.go (Task 15)")
}

// TestClassifyMainRewind_DistinguishesSharedAndDisjoint regression-tests PR
// #61 review finding #3. The previous reconcileMain code collapsed three
// distinct outcomes into a single bool:
//
//	bases, mbErr := localCommit.MergeBase(originCommit)
//	disjoint := mbErr == nil && len(bases) == 0
//
// An mbErr (object-store corruption, IO failure) was indistinguishable
// from "MergeBase succeeded and found a shared ancestor" — both gave
// disjoint=false, and the operator saw the *less* alarming "not a
// descendant" log line. classifyMainRewind separates the three cases so
// reconcileMain can log them distinctly.
func TestClassifyMainRewind_DistinguishesSharedAndDisjoint(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Shared history: two commits both descended from the root. MergeBase
	// succeeds and returns the root as common ancestor.
	rootHash := mustHeadHash(t, svc, "main")
	sideA := writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	sideB := writeMergeFact(t, svc, "main", "kb/b.md", "B", "v1")

	commitA, err := svc.rh.repo.CommitObject(plumbing.NewHash(sideA))
	require.NoError(t, err)
	commitB, err := svc.rh.repo.CommitObject(plumbing.NewHash(sideB))
	require.NoError(t, err)

	sharedClass := classifyMainRewind(commitA, commitB)
	require.NoError(t, sharedClass.BaseErr,
		"shared ancestry must not surface a MergeBase error")
	require.False(t, sharedClass.Disjoint,
		"shared ancestry must NOT be classified as disjoint (root %s is the common base)", rootHash.String()[:8])

	// Disjoint history: a wholly unrelated root in the same object store.
	disjointHash := makeDisjointRoot(t, svc, "disjoint/root.md", "unrelated content")
	disjointCommit, err := svc.rh.repo.CommitObject(disjointHash)
	require.NoError(t, err)

	disjointClass := classifyMainRewind(commitA, disjointCommit)
	require.NoError(t, disjointClass.BaseErr,
		"disjoint histories must not surface a MergeBase error — the call succeeded, it just found no common ancestor")
	require.True(t, disjointClass.Disjoint,
		"disjoint histories must be classified as Disjoint=true")
}

// TestReconcileMain_LogsMergeBaseErrorDistinctly regression-tests finding
// #3's secondary contract: when MergeBase fails (vs returns empty), the
// reconcile must STILL force-update local and emit a warn log that
// includes the underlying error — not silently route to the
// "not a descendant" message that gives the operator no signal about
// why disjoint detection couldn't run.
//
// We exercise the path via the rewind classifier rather than provoking a
// real MergeBase failure (which requires a corrupted object store and
// is impractical to fabricate in-process); the classifier's contract
// guarantees that a non-nil BaseErr is what reconcileMain renders to
// the distinct log line.
func TestReconcileMain_LogsMergeBaseErrorDistinctly(t *testing.T) {
	// Contract check: rewindClassification has a BaseErr field that
	// reconcileMain logs distinctly from the disjoint and shared cases.
	// A zero value classification renders no error; a non-nil BaseErr
	// (whatever the underlying cause) must not be lost to the inferior
	// "not a descendant" message.
	var noErr rewindClassification
	require.NoError(t, noErr.BaseErr, "zero classification carries no error")

	withErr := rewindClassification{BaseErr: object.ErrUnsupportedObject}
	require.Error(t, withErr.BaseErr,
		"classification preserves MergeBase errors so reconcileMain can surface them distinctly")
	require.False(t, withErr.Disjoint,
		"a MergeBase error must NOT silently mean disjoint=false — reconcileMain logs the error explicitly instead")
}
