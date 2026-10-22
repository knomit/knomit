package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/merge"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/platform/fileuri"
)

// The consensus branch of a repo with no origin is the RECORDED one, never a
// default name (remotesync/consensus_branch.go).
//
// SABOTAGE: localConsensusBranch returning the literal "main" → the
// "trunk" cases go red.
func TestConsensusBranch_RecordedNotDefaulted(t *testing.T) {
	ctx := context.Background()

	t.Run("local init records the branch it creates, across a reopen", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "k.db")
		svc, err := Open(path)
		require.NoError(t, err)
		require.NoError(t, svc.InitRepoWithUpstream(context.Background(), map[string]string{}, "trunk", "agent/host-1"))
		require.Equal(t, "trunk", svc.Remote().UpstreamBranch())
		require.NoError(t, svc.Close())

		svc, err = Open(path)
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.OpenRepo())
		require.Equal(t, "trunk", svc.Remote().UpstreamBranch(), "read back from the repo database, not a default")
	})

	t.Run("removing the origin keeps its branch", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.InitRepoWithUpstream(context.Background(), map[string]string{}, "trunk", "agent/host-1"))
		svc.Remote().SetOrigin(&remotesync.Origin{URL: "https://example.invalid/kb.git", Branch: "develop"})
		require.Equal(t, "develop", svc.Remote().UpstreamBranch())
		svc.Remote().SetOrigin(nil)
		require.Equal(t, "develop", svc.Remote().UpstreamBranch(), "the origin went; its branch stays the consensus branch")
	})

	t.Run("a repo written before the name was recorded resolves its one non-agent branch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "k.db")
		svc, err := Open(path)
		require.NoError(t, err)
		require.NoError(t, svc.InitRepoWithUpstream(context.Background(), map[string]string{}, "trunk", "agent/host-1"))
		// Forget the record, as on a repo from an older binary, then reopen so
		// the fresh index starts with an empty cache (init cached "trunk");
		// add a pushed peer branch and an experiment, which are never candidates.
		_, err = svc.rh.DB().Exec(`DELETE FROM meta WHERE key = ?`, remotesync.ConsensusBranchKey)
		require.NoError(t, err)
		require.NoError(t, svc.Close())
		svc, err = Open(path)
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.OpenRepo())
		tip, err := svc.HeadCommit(ctx, "agent/host-1")
		require.NoError(t, err)
		for _, b := range []string{"agent/peer-abcd1234", "exp/try"} {
			require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), plumbing.NewHash(tip))))
		}
		require.Equal(t, "trunk", svc.Remote().UpstreamBranch())
		require.Equal(t, "trunk", svc.Remote().RecordedConsensusBranch(), "and records it")
	})
}

// A clone and a subscription RECORD the consensus branch they resolved, on
// success, before the caller does anything else (issue #566). The reader
// (UpstreamBranch) would hide a missing record: with a single non-agent branch
// it scans refs/heads and records the answer itself, and it answers from the
// cache the record fills. So the test reads the meta row that is the record
// (RecordedConsensusBranch), straight after the call, with no SetOrigin and no
// UpstreamBranch before it.
//
// SABOTAGE: delete the deferred ri.RecordConsensusBranch(upstream) in
// remotesync.Index.InitFromRemote → the clone subtests (and the empty-remote
// one) go red; delete ri.RecordConsensusBranch(upstreamMain) in InitSubscription
// → the subscription subtest goes red.
func TestInitFromRemote_RecordsTheResolvedConsensusBranchBeforeAnySetOrigin(t *testing.T) {
	open := func(t *testing.T) *Service {
		t.Helper()
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.Empty(t, svc.Remote().RecordedConsensusBranch(), "a fresh store records nothing")
		return svc
	}

	t.Run("clone records the branch it resolved", func(t *testing.T) {
		svc := open(t)
		up, _, err := svc.Remote().InitFromRemote(context.Background(), fileuri.New(bareRemoteWith(t, "trunk", "trunk")), nil, "", "agent/test", nil, nil)
		require.NoError(t, err)
		require.Equal(t, "trunk", up)
		require.Equal(t, "trunk", svc.Remote().RecordedConsensusBranch())
	})
	t.Run("clone records the branch it was asked for", func(t *testing.T) {
		svc := open(t)
		up, _, err := svc.Remote().InitFromRemote(context.Background(), fileuri.New(bareRemoteWith(t, "trunk", "main", "trunk")), nil, "main", "agent/test", nil, nil)
		require.NoError(t, err)
		require.Equal(t, "main", up)
		require.Equal(t, "main", svc.Remote().RecordedConsensusBranch())
	})
	t.Run("a clone of an empty remote records the branch it created", func(t *testing.T) {
		bare := t.TempDir()
		mustRun(t, "", "git", "init", "--bare", bare)
		svc := open(t)
		up, wasEmpty, err := svc.Remote().InitFromRemote(context.Background(), fileuri.New(bare), nil, "trunk", "agent/test", nil, nil)
		require.NoError(t, err)
		require.True(t, wasEmpty)
		require.Equal(t, "trunk", up)
		require.Equal(t, "trunk", svc.Remote().RecordedConsensusBranch())
	})
	t.Run("a refused clone records nothing", func(t *testing.T) {
		svc := open(t)
		_, _, err := svc.Remote().InitFromRemote(context.Background(), fileuri.New(bareRemoteWith(t, "agent/other", "main", "trunk", "agent/other")), nil, "", "agent/test", nil, nil)
		require.ErrorIs(t, err, model.ErrNoConsensusBranch)
		require.Empty(t, svc.Remote().RecordedConsensusBranch(), "the record is written on success only")
	})
	t.Run("subscription records the branch it resolved", func(t *testing.T) {
		svc := open(t)
		up, err := svc.Remote().InitSubscription(context.Background(), fileuri.New(bareRemoteWith(t, "trunk", "trunk")), nil, "", nil)
		require.NoError(t, err)
		require.Equal(t, "trunk", up)
		require.Equal(t, "trunk", svc.Remote().RecordedConsensusBranch())
	})
}

// The refusals the Service used to do for MainHistory and JudgePeerBranch now
// live in the remotesync.Index methods and run first: a repo whose consensus
// branch cannot be determined answers ErrNoConsensusBranch, before the shape
// checks and before anything is resolved. DemotePeerBranch refuses nothing for
// an empty consensus branch (kb/invariants/store/consensus-branch-recorded):
// with "" only the agent/ prefix and own are excluded.
//
// SABOTAGE: delete the `upstream == ""` refusal in Index.MainHistory, or in
// Index.JudgePeerBranch → the matching subtest goes red (JudgePeerBranch's
// answers the shape error instead). Add one to DemotePeerBranch → its subtest
// goes red.
func TestRemote_EmptyConsensusBranchRefusals(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.Empty(t, svc.Remote().UpstreamBranch(), "fixture: a store with no refs and no origin names no consensus branch")

	tip := plumbing.NewHash("1111111111111111111111111111111111111111")

	t.Run("MainHistory refuses", func(t *testing.T) {
		h, err := svc.Remote().MainHistory(ctx)
		require.ErrorIs(t, err, model.ErrNoConsensusBranch)
		require.Nil(t, h)
	})
	t.Run("JudgePeerBranch refuses before its shape checks", func(t *testing.T) {
		// "main" is not an agent/* branch, which the shape check would refuse
		// with its own text; the consensus-branch refusal comes first.
		_, err := svc.Remote().JudgePeerBranch(ctx, "main", "agent/me", tip, nil)
		require.ErrorIs(t, err, model.ErrNoConsensusBranch)
		_, err = svc.Remote().JudgePeerBranch(ctx, "agent/peer", "agent/me", plumbing.ZeroHash, nil)
		require.ErrorIs(t, err, model.ErrNoConsensusBranch, "and before the zero-tip check")
	})
	t.Run("AdmitPeerBranch refuses through the judge", func(t *testing.T) {
		_, err := svc.Remote().AdmitPeerBranch(ctx, "agent/peer", "agent/me", tip, nil)
		require.ErrorIs(t, err, model.ErrNoConsensusBranch)
	})
	t.Run("DemotePeerBranch does not refuse", func(t *testing.T) {
		// An agent/* branch nobody registered: nothing to demote, and no
		// consensus-branch error.
		ok, err := svc.Remote().DemotePeerBranch(ctx, "agent/peer", "agent/me", tip)
		require.NoError(t, err)
		require.False(t, ok)
		// The shape check still applies, with its own text.
		_, err = svc.Remote().DemotePeerBranch(ctx, "main", "agent/me", tip)
		require.Error(t, err)
		require.NotErrorIs(t, err, model.ErrNoConsensusBranch)
	})
	t.Run("EnsureLocalUpstream and AdvanceLocalUpstream take an empty upstream as a no-op", func(t *testing.T) {
		created, err := svc.Remote().EnsureLocalUpstream(ctx, "")
		require.NoError(t, err)
		require.False(t, created)
		res, err := svc.Remote().AdvanceLocalUpstream(ctx, "agent/me", "")
		require.NoError(t, err)
		require.Equal(t, merge.ModeNoop, res.Mode)
	})
}
