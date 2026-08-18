package repos

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestCreate_localWritesOntologyToAgentBranch verifies that a brand new repo
// with no origin lands the default ontology on the agent branch, where
// loadOntology looks for it.
func TestCreate_localWritesOntologyToAgentBranch(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test-abc",
	})
	ri := bootRepo(t, m)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", OntologyPath, nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

// The clone-of-an-EMPTY-remote case used to be tested here, pinning that clone
// mode seeded fact.DefaultOntology() onto the agent branch so such a repo was
// not left ontology-less. That behaviour is GONE, deliberately, and its
// replacement is a refusal — see TestCreate_CloneRefusesAnEmptyRemote and
// TestCreate_InitializeRefusesAnEmptyRemote in lifecycle_initialize_test.go.
//
// Two things were wrong with it. An empty remote has no branch to cut an agent
// branch from, so InitFromRemote's empty path minted a fresh ROOT COMMIT — a
// repo identity no other machine sharing that remote would ever agree with.
// And the ontology it seeded was the DEFAULT, chosen by the code rather than
// the user, which is the same silent substitution that made a reader who
// picked "Code" end up with "General". A repository is a knomit knowledge base
// if and only if it has an ontology; deciding that on the user's behalf is
// precisely what this design removed.

// TestCreate_LocalOriginRejectedWithoutRoot pins the absolute local-origin
// policy at the clone path: a filesystem origin is rejected when
// local_origin_root is unset — filesystem origins are unavailable everywhere.
// Without this gate a user would get a one-time snapshot from a path the sync
// loop and recover paths then refuse to touch, so it would silently stop
// syncing.
func TestCreate_LocalOriginRejectedWithoutRoot(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home: dir,
			// LocalOriginRoot intentionally empty → filesystem origins disabled.
		},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	_, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.Error(t, err, "a file:// origin must be rejected when local_origin_root is unset")
	require.Contains(t, err.Error(), "local-path origins are disabled")
	require.Nil(t, m.Get(testRepoName), "a rejected clone must leave no repo registered")
}

// TestCreate_LocalOriginRejectedOutsideRoot pins that even with a root set, an
// origin outside it is rejected.
func TestCreate_LocalOriginRejectedOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir() // a sibling that is not under the configured root

	remoteDir := filepath.Join(outside, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home:            dir,
			LocalOriginRoot: filepath.Join(dir, "allowed"),
		},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	_, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.Error(t, err, "a file:// origin outside local_origin_root must be rejected")
	require.Contains(t, err.Error(), "outside the allowed root")
	require.Nil(t, m.Get(testRepoName), "a rejected clone must leave no repo registered")
}
