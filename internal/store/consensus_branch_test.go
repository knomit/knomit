package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// The consensus branch of a repo with no origin is the RECORDED one, never a
// default name (consensus_branch.go).
//
// SABOTAGE: localConsensusBranch returning the literal "main" → the
// "trunk" cases go red.
func TestConsensusBranch_RecordedNotDefaulted(t *testing.T) {
	ctx := context.Background()

	t.Run("local init records the branch it creates, across a reopen", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "k.db")
		svc, err := Open(path)
		require.NoError(t, err)
		require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "trunk", "agent/host-1"))
		require.Equal(t, "trunk", svc.UpstreamBranch())
		require.NoError(t, svc.Close())

		svc, err = Open(path)
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.OpenRepo())
		require.Equal(t, "trunk", svc.UpstreamBranch(), "read back from the repo database, not a default")
	})

	t.Run("removing the origin keeps its branch", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "trunk", "agent/host-1"))
		svc.SetOrigin(&Origin{URL: "https://example.invalid/kb.git", Branch: "develop"})
		require.Equal(t, "develop", svc.UpstreamBranch())
		svc.SetOrigin(nil)
		require.Equal(t, "develop", svc.UpstreamBranch(), "the origin went; its branch stays the consensus branch")
	})

	t.Run("a repo written before the name was recorded resolves its one non-agent branch", func(t *testing.T) {
		svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
		require.NoError(t, err)
		defer svc.Close()
		require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "trunk", "agent/host-1"))
		// Forget the record, as on a repo from an older binary; add a pushed
		// peer branch and an experiment, which are never candidates.
		_, err = svc.rh.db.Exec(`DELETE FROM meta WHERE key = ?`, consensusBranchKey)
		require.NoError(t, err)
		svc.ri.setConsensus("")
		tip, err := svc.Branches().HeadCommit(ctx, "agent/host-1")
		require.NoError(t, err)
		for _, b := range []string{"agent/peer-abcd1234", "exp/try"} {
			require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(b), plumbing.NewHash(tip))))
		}
		require.Equal(t, "trunk", svc.UpstreamBranch())
		require.Equal(t, "trunk", svc.recordedConsensusBranch(), "and records it")
	})
}
