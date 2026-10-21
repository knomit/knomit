package store

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"knomit/internal/platform/fileuri"
	"knomit/internal/store/merge"
	"knomit/internal/store/remotesync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// A repository with NO watermark (made before refs/knomit/agent-base existed
// and not synced since) used to take the merge base as the rewind replay's
// stop point. An origin reset of main to an ANCESTOR of the agent branch then
// read as "the branch is strictly ahead": the commit the origin removed was
// kept and the next push sent it back. Sync now seeds the missing watermark
// from where origin/main was before its fetch moved it (the last origin
// commit this copy synced with). These tests run the whole Sync against a
// real bare remote, since that position can only be read before the fetch.

// wmFixture is a store cloned from a bare remote whose main is the seed
// commit r. Returns the store, the bare remote's path and r.
func wmFixture(t *testing.T) (*Service, string, plumbing.Hash) {
	t.Helper()
	bare := seedBareRemoteOnMain(t)
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	_, _, err = svc.InitFromRemote(context.Background(), fileuri.New(bare), nil, "main", "agent/test", nil, nil)
	require.NoError(t, err)
	svc.SetOrigin(&Origin{URL: fileuri.New(bare), Branch: "main"})
	r := mustHeadHash(t, svc, "main")
	return svc, bare, r
}

// wmSync runs one Sync and returns its agent result.
func wmSync(t *testing.T, svc *Service) SyncResult {
	t.Helper()
	res, err := svc.Remote().(*remotesync.Index).Sync(context.Background(), "agent/test", nil)
	require.NoError(t, err)
	return res
}

// wmPush pushes the agent branch to the bare remote.
func wmPush(t *testing.T, svc *Service) {
	t.Helper()
	_, err := svc.Remote().(*remotesync.Index).Push(context.Background(), "agent/test", nil)
	require.NoError(t, err)
}

// wmSetOriginMain force-moves the bare remote's main to h.
func wmSetOriginMain(t *testing.T, bare string, h plumbing.Hash) {
	t.Helper()
	mustRun(t, bare, "git", "update-ref", "refs/heads/main", h.String())
}

// wmDropWatermark removes the agent branch's watermark: the repository now
// looks like one made before the watermark existed.
func wmDropWatermark(t *testing.T, svc *Service) {
	t.Helper()
	require.NoError(t, svc.rh.Storer().RemoveReference(remotesync.AgentBaseRefNameForTest("agent/test")))
	_, err := svc.rh.ReadAgentBaseForTest("agent/test")
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "setup: no watermark")
}

// wmOriginHolds reports whether the bare remote's branch holds h.
func wmOriginHolds(t *testing.T, bare, branch string, h plumbing.Hash) bool {
	t.Helper()
	c := exec.Command("git", "merge-base", "--is-ancestor", h.String(), "refs/heads/"+branch)
	c.Dir = bare
	out, err := c.CombinedOutput()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit) && exit.ExitCode() == 1, "git merge-base failed: %v\n%s", err, out)
	return false
}

// wmConsumeSecret writes kb/secret.md on the agent branch, pushes it, has the
// origin take it into main, and syncs, so this copy consumed it (main is s).
// The watermark is then dropped. Returns s.
func wmConsumeSecret(t *testing.T, svc *Service, bare string) plumbing.Hash {
	t.Helper()
	s := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/secret.md", "Secret", "scrub me"))
	wmPush(t, svc)
	wmSetOriginMain(t, bare, s)
	wmSync(t, svc)
	require.Equal(t, s, mustHeadHash(t, svc, "main"), "setup: this copy consumed s")
	wmDropWatermark(t, svc)
	return s
}

// The scrub: the origin resets main BACK to r, an ancestor of the branch,
// to remove s. With no watermark the branch read as strictly ahead and kept s.
// SABOTAGE: never seed (seedMissingWatermark returns at once) -> s kept -> red.
func TestSync_NoWatermarkResetToAnAncestorDoesNotKeepTheRemovedCommit(t *testing.T) {
	t.Run("nothing else", func(t *testing.T) {
		svc, bare, r := wmFixture(t)
		s := wmConsumeSecret(t, svc, bare)

		wmSetOriginMain(t, bare, r)
		res := wmSync(t, svc)
		require.Equal(t, merge.ModeRewound, res.Main.Mode)
		require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"), "the removed file stays removed")
		require.False(t, reachableFrom(t, svc, "agent/test", s), "the removed commit is not on the branch")
		base, err := svc.rh.ReadAgentBaseForTest("agent/test")
		require.NoError(t, err)
		require.Equal(t, r, base, "the watermark is written, so the fallback is used once")

		wmPush(t, svc)
		require.False(t, wmOriginHolds(t, bare, "agent/test", s), "the push does not send the removed commit back")
	})
	// SABOTAGE: seed the watermark at the agent branch's tip (count every
	// unsynced commit as consumed) -> later.md is dropped -> red.
	t.Run("an own unsynced write on top is kept", func(t *testing.T) {
		svc, bare, r := wmFixture(t)
		s := wmConsumeSecret(t, svc, bare)
		writeMergeFact(t, svc, "agent/test", "kb/later.md", "Later", "not synced yet")

		wmSetOriginMain(t, bare, r)
		res := wmSync(t, svc)
		require.Equal(t, merge.ModeRewound, res.Main.Mode)
		require.Equal(t, merge.ModeRebase, res.Agent.Mode)
		require.Equal(t, 1, res.Agent.NumReplayed)
		require.True(t, hasFileAt(t, svc, "agent/test", "kb/later.md"), "the own unsynced write is kept")
		require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"))
		require.False(t, reachableFrom(t, svc, "agent/test", s))
	})
}

// The seed writes ONLY a missing watermark; an existing one is never
// overwritten (#542 review SF1). This copy consumed s, so its watermark is s.
// The tracking ref then moved past it to x, a foreign commit on s (a fetch
// whose reconcile failed). The origin then resets main to r. The real
// watermark s stops the walk, so s is not kept, and the own write b is kept.
// Overwritten with x, the walk would run past s down to r and replay s.
// SABOTAGE: the seed overwrites an existing watermark (drop the "already
// there" return) -> secret.md is kept -> red.
func TestSync_ExistingWatermarkIsNotOverwritten(t *testing.T) {
	svc, bare, r := wmFixture(t)
	s := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/secret.md", "Secret", "scrub me"))
	wmPush(t, svc)
	wmSetOriginMain(t, bare, s)
	wmSync(t, svc)
	base, err := svc.rh.ReadAgentBaseForTest("agent/test")
	require.NoError(t, err)
	require.Equal(t, s, base, "setup: the watermark is s")

	x := upstreamCommit(t, svc, s, "kb/x.md", obsFact("X"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), x)))
	writeMergeFact(t, svc, "agent/test", "kb/b.md", "B", "own, not synced yet")

	wmSetOriginMain(t, bare, r)
	res := wmSync(t, svc)
	require.Equal(t, merge.ModeRewound, res.Main.Mode)
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/b.md"), "the own unsynced write is kept")
	require.False(t, hasFileAt(t, svc, "agent/test", "kb/secret.md"), "the real watermark s stops the walk; s is not kept")
	require.False(t, reachableFrom(t, svc, "agent/test", s))
}

// A branch genuinely ahead of the origin: its own write w sits on r, which is
// where the origin was at the last fetch and still is. Local main was moved
// past the origin locally (here set directly), so the main reconcile reports
// a rewind. Nothing the origin removed is on the branch, so nothing is dropped
// and its hashes stay as they are.
// SABOTAGE: seed the watermark at the agent branch's tip -> w is dropped by
// the rebuild -> red.
func TestSync_NoWatermarkGenuinelyAheadDropsNothing(t *testing.T) {
	svc, _, r := wmFixture(t)
	w := plumbing.NewHash(writeMergeFact(t, svc, "agent/test", "kb/w.md", "W", "not synced yet"))
	local := upstreamCommit(t, svc, r, "kb/local-main.md", obsFact("Local main"))
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), local)))
	wmDropWatermark(t, svc)

	res := wmSync(t, svc)
	require.Equal(t, merge.ModeRewound, res.Main.Mode, "setup: the main reconcile reports a rewind")
	require.Equal(t, w, mustHeadHash(t, svc, "agent/test"), "nothing dropped, hashes unchanged")
	require.True(t, hasFileAt(t, svc, "agent/test", "kb/w.md"))
}

// A truly first sync: no watermark AND no remote-tracking position before the
// fetch. There is no last origin commit to fall back to, so the replay keeps
// today's merge-base fallback: a reset to an ancestor still reads as strictly
// ahead and the branch is kept as it is (the residual this cannot close).
// SABOTAGE: with no remote-tracking ref, seed from local main instead -> the
// branch is rebuilt on r -> red.
func TestSync_FirstSyncEverKeepsTheMergeBaseFallback(t *testing.T) {
	svc, bare, r := wmFixture(t)
	s := wmConsumeSecret(t, svc, bare)
	require.NoError(t, svc.rh.Storer().RemoveReference(plumbing.NewRemoteReferenceName("origin", "main")))

	wmSetOriginMain(t, bare, r)
	res := wmSync(t, svc)
	require.Equal(t, merge.ModeRewound, res.Main.Mode)
	require.Equal(t, merge.ModeNoop, res.Agent.Mode, "unchanged: the merge-base fallback reads the branch as ahead")
	require.Equal(t, s, mustHeadHash(t, svc, "agent/test"))
	base, err := svc.rh.ReadAgentBaseForTest("agent/test")
	require.NoError(t, err)
	require.Equal(t, r, base, "the reconcile still writes the watermark afterwards")
}
