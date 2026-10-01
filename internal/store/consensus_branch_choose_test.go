package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/stretchr/testify/require"

	"knomit/internal/platform/fileuri"
)

// bareRemoteWith builds a bare remote holding one commit on each of branches
// (each its own commit, so their tips differ) with HEAD pointing at head.
func bareRemoteWith(t *testing.T, head string, branches ...string) string {
	t.Helper()
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch="+branches[0], bareDir)
	work := t.TempDir()
	mustRun(t, "", "git", "clone", bareDir, work)
	mustRun(t, work, "git", "config", "user.email", "t@t")
	mustRun(t, work, "git", "config", "user.name", "t")
	for _, b := range branches {
		mustRun(t, work, "git", "checkout", "-B", b)
		require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed "+b), 0o644))
		mustRun(t, work, "git", "add", "seed.txt")
		mustRun(t, work, "git", "commit", "-m", "seed "+b)
		mustRun(t, work, "git", "push", "origin", b)
	}
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/"+head)
	return bareDir
}

func TestChooseConsensusBranch(t *testing.T) {
	for _, tc := range []struct {
		name, requested, head string
		branches              []string
		want                  string
	}{
		{"a request wins", "develop", "trunk", []string{"main", "trunk"}, "develop"},
		{"HEAD wins over a branch called main", "", "trunk", []string{"main", "trunk"}, "trunk"},
		{"HEAD on master", "", "master", []string{"master"}, "master"},
		{"an agent-branch HEAD is skipped for the one other branch", "", "agent/x", []string{"agent/x", "trunk"}, "trunk"},
		{"an agent-branch HEAD with two candidates is undecidable", "", "agent/x", []string{"agent/x", "main", "trunk"}, ""},
		{"no HEAD, one candidate", "", "", []string{"develop", "exp/try", "okf/gen"}, "develop"},
		{"no HEAD, two candidates", "", "", []string{"main", "master"}, ""},
		{"HEAD names a branch that was not fetched", "", "gone", []string{"main", "trunk"}, ""},
		{"only role branches", "", "agent/x", []string{"agent/x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ChooseConsensusBranch(tc.requested, tc.head, tc.branches))
		})
	}
}

// A remote holding BOTH main and trunk, with HEAD on trunk: the clone and the
// subscription adopt trunk. The old rule preferred "main" by name.
//
// SABOTAGE: restore the "prefer main" step in resolveUpstream → both subtests
// adopt main → red.
func TestResolveUpstream_HeadNotMainByName(t *testing.T) {
	bare := bareRemoteWith(t, "trunk", "main", "trunk")
	assertTrunk := func(t *testing.T, svc *Service, upstream string) {
		t.Helper()
		require.Equal(t, "trunk", upstream)
		ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("trunk"))
		require.NoError(t, err)
		require.NotEqual(t, plumbing.ZeroHash, ref.Hash())
		require.Equal(t, "trunk", svc.UpstreamBranch())
	}

	t.Run("clone", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		up, _, err := svc.InitFromRemote(fileuri.New(bare), nil, "", "agent/test", nil, nil)
		require.NoError(t, err)
		assertTrunk(t, svc, up)
	})
	t.Run("subscription", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		up, err := svc.InitSubscription(fileuri.New(bare), nil, "", nil)
		require.NoError(t, err)
		assertTrunk(t, svc, up)
	})
}

// A remote whose HEAD is an agent branch and which holds two other branches
// names no consensus branch: the clone is refused, asking for one.
//
// SABOTAGE: restore "prefer main" → the clone adopts main → red.
func TestResolveUpstream_UndecidableIsRefused(t *testing.T) {
	bare := bareRemoteWith(t, "agent/other", "main", "trunk", "agent/other")
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	_, _, err = svc.InitFromRemote(fileuri.New(bare), nil, "", "agent/test", nil, nil)
	require.ErrorIs(t, err, ErrNoConsensusBranch)
	require.Contains(t, err.Error(), "trunk", "the refusal lists the candidates")
}

// The seed path (an empty remote) CREATES the consensus branch: a requested
// name is used as given, and only an unnamed one gets DefaultConsensusBranch.
// A local create gets DefaultConsensusBranch too, and records it.
func TestNewRepo_DefaultConsensusBranchOnlyWhenUnnamed(t *testing.T) {
	t.Run("empty remote, trunk requested", func(t *testing.T) {
		bare := t.TempDir()
		mustRun(t, "", "git", "init", "--bare", bare)
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		up, wasEmpty, err := svc.InitFromRemote(fileuri.New(bare), nil, "trunk", "agent/test", nil, nil)
		require.NoError(t, err)
		require.True(t, wasEmpty)
		require.Equal(t, "trunk", up)
		require.Equal(t, "trunk", svc.UpstreamBranch())
	})
	t.Run("empty remote, nothing requested", func(t *testing.T) {
		bare := t.TempDir()
		mustRun(t, "", "git", "init", "--bare", bare)
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		up, _, err := svc.InitFromRemote(fileuri.New(bare), nil, "", "agent/test", nil, nil)
		require.NoError(t, err)
		require.Equal(t, DefaultConsensusBranch, up)
	})
	t.Run("local create", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
		require.Equal(t, DefaultConsensusBranch, svc.UpstreamBranch())
		require.Equal(t, DefaultConsensusBranch, svc.recordedConsensusBranch())
	})
}

// The two public ways into the fetch/reconcile paths refuse an empty branch,
// which is why the paths below them carry no default of their own.
//
// SABOTAGE: restore Sync's `upstreamMain = "main"` → Sync on a repo with no git
// remote returns nil ("no origin git remote") instead of the refusal → red.
// SABOTAGE: delete ConfigureRemote's refusal → it writes an origin remote with
// a "+refs/heads/:..." refspec → red.
func TestSyncEntries_RefuseAnEmptyConsensusBranch(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "trunk", "agent/test"))

	err = svc.ConfigureRemote("https://example.invalid/kb.git", "", "agent/test")
	require.ErrorIs(t, err, ErrNoConsensusBranch)
	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	require.NotContains(t, cfg.Remotes, "origin", "nothing is configured for a refused branch")

	svc.SetOrigin(&Origin{URL: "https://example.invalid/kb.git"})
	_, err = svc.Remote().Sync(context.Background(), "agent/test", nil)
	require.ErrorIs(t, err, ErrNoConsensusBranch)
}

// Replay with no DefaultBranch cuts the agent branch from the target clone's
// HEAD branch (the remote's default), not from a branch called main.
//
// SABOTAGE: restore `cfg.DefaultBranch = "main"` → the agent branch is cut from
// main's tip → red.
func TestReplay_UnsetDefaultBranchIsTheTargetsHead(t *testing.T) {
	ctx := context.Background()
	const agent = "agent/laptop"

	local, err := Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	require.NoError(t, local.InitRepo(map[string]string{}, agent))
	_, err = local.Facts().WriteFact(ctx, agent, "kb/a.md", testFactBody("A", 0.9, nil), "a", "")
	require.NoError(t, err)

	target, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, target.InitRepoWithUpstream(map[string]string{}, "trunk", "agent/other"))
	// A "main" that is NOT the remote's default, at a different commit.
	_, err = target.Facts().WriteFact(ctx, "agent/other", "kb/x.md", testFactBody("X", 0.9, nil), "x", "")
	require.NoError(t, err)
	other, err := target.rh.gits.Reference(plumbing.NewBranchReferenceName("agent/other"))
	require.NoError(t, err)
	require.NoError(t, target.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), other.Hash())))
	require.NoError(t, target.Branches().SetDefaultBranch("trunk"), "a clone's HEAD is the remote's default branch")
	trunk, err := target.rh.gits.Reference(plumbing.NewBranchReferenceName("trunk"))
	require.NoError(t, err)

	iter, err := local.Search().FactsIter(ctx, agent)
	require.NoError(t, err)
	_, err = Replay(ctx, local, agent, iter, target, ReplayConfig{Strategy: StrategyLocalWins, AgentBranch: agent, SkipIndexSync: true})
	require.NoError(t, err)

	repo, err := gogit.Open(target.rh.gits, nil)
	require.NoError(t, err)
	tip, err := target.rh.gits.Reference(plumbing.NewBranchReferenceName(agent))
	require.NoError(t, err)
	isAncestor := func(anc plumbing.Hash) bool {
		c, err := repo.CommitObject(tip.Hash())
		require.NoError(t, err)
		found := false
		require.NoError(t, object.NewCommitPreorderIter(c, nil, nil).ForEach(func(x *object.Commit) error {
			if x.Hash == anc {
				found = true
				return storer.ErrStop
			}
			return nil
		}))
		return found
	}
	require.True(t, isAncestor(trunk.Hash()), "the agent branch starts from trunk, the target's HEAD")
	require.False(t, isAncestor(other.Hash()), "and not from the branch called main")
}
