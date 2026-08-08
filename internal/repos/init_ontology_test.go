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

// TestCreate_cloneOfEmptyRemoteWritesOntology verifies that cloning an empty
// remote (no branches yet) still seeds the ontology onto the agent branch.
// There is nothing to clone, so the seed files are the only source — a repo
// created this way must not end up ontology-less while every locally-created
// repo has one.
func TestCreate_cloneOfEmptyRemoteWritesOntology(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home: dir,
			// A filesystem origin is only permitted inside LocalOriginRoot; the
			// remote lives under dir, so allow that root.
			LocalOriginRoot: dir,
		},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	ri, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", OntologyPath, nil)
	require.NoError(t, err, "ontology must be readable from agent branch after clone of an empty remote")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

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
