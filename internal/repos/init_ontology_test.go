package repos

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestCreate_PresetWritesOntologyToAgentBranch verifies that a newly created
// repo with no origin ends up with the default ontology on the agent branch so
// that loadOntology can find it.
func TestCreate_PresetWritesOntologyToAgentBranch(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test-abc",
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())
	ri := mustCreateRepo(t, m, testRepoName)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

// TestCreate_CloneFromEmptyRemoteWritesOntology verifies that a repo created
// against an EMPTY remote (no branches yet) still gets an ontology on its agent
// branch. There is nothing to clone, so initClone's seed files are what supply
// it — a non-empty origin brings its own and ignores them.
func TestCreate_CloneFromEmptyRemoteWritesOntology(t *testing.T) {
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
		AgentBranch: "agent/test-abc",
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())

	ri, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init from empty remote")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

// TestCreate_LocalOriginRejectedWithoutRoot pins the absolute local-origin
// policy on the create path: a filesystem origin is rejected when
// local_origin_root is unset — filesystem origins are unavailable everywhere.
// The gap this guards is a one-time snapshot from a file:// origin that then
// silently stops syncing, because the sync loop and recover paths DO block it.
func TestCreate_LocalOriginRejectedWithoutRoot(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		// LocalOriginRoot intentionally empty → filesystem origins disabled.
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test-abc",
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())

	_, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.Error(t, err, "a file:// origin must be rejected when local_origin_root is unset")
	require.Contains(t, err.Error(), "local-path origins are disabled")
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
		AgentBranch: "agent/test-abc",
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())

	_, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.Error(t, err, "a file:// origin outside local_origin_root must be rejected")
	require.Contains(t, err.Error(), "outside the allowed root")
}
