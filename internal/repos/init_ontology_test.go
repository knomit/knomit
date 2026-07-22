package repos

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestBoot_firstRunWritesOntologyToAgentBranch verifies that on a brand new
// repo (no origin configured), the default ontology ends up on the agent
// branch so that loadOntology can find it.
func TestBoot_firstRunWritesOntologyToAgentBranch(t *testing.T) {
	dir := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test-abc",
	})
	err := m.Start()
	require.NoError(t, err)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

// TestBoot_firstRunWithEmptyRemoteWritesOntology verifies that when the
// default repo is initialised against an empty remote (no branches yet),
// the ontology is written to the agent branch.
func TestBoot_firstRunWithEmptyRemoteWritesOntology(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home: dir,
			Git:  config.GitConfig{Origin: "file://" + remoteDir},
			// A filesystem origin is only permitted inside LocalOriginRoot; the
			// remote lives under dir, so allow that root.
			LocalOriginRoot: dir,
		},
		AgentBranch: "agent/test-abc",
	})
	err := m.Start()
	require.NoError(t, err)
	ri := m.Get(config.DefaultRepoName)
	require.NotNil(t, ri)

	result, err := testService(t, ri).Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init from empty remote")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}

// TestBoot_LocalConfigOriginRejectedWithoutRoot pins the absolute local-origin
// policy at the config-boot path: a filesystem origin set in the operator's
// config (git.origin = file://…) is rejected when local_origin_root is unset —
// filesystem origins are unavailable everywhere, the config file included. This
// is the regression test for the gap where initDefaultGit cloned a file:// config
// origin ungated while the sync loop and recover paths blocked it, so an operator
// got a one-time snapshot that then silently stopped syncing.
func TestBoot_LocalConfigOriginRejectedWithoutRoot(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home: dir,
			Git:  config.GitConfig{Origin: "file://" + remoteDir},
			// LocalOriginRoot intentionally empty → filesystem origins disabled.
		},
		AgentBranch: "agent/test-abc",
	})
	err := m.Start()
	require.Error(t, err, "a file:// config origin must be rejected when local_origin_root is unset")
	require.Contains(t, err.Error(), "local-path origins are disabled")
}

// TestBoot_LocalConfigOriginRejectedOutsideRoot pins that even with a root set, a
// config origin outside it is rejected at boot.
func TestBoot_LocalConfigOriginRejectedOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir() // a sibling that is not under the configured root

	remoteDir := filepath.Join(outside, "remote.git")
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home:            dir,
			Git:             config.GitConfig{Origin: "file://" + remoteDir},
			LocalOriginRoot: filepath.Join(dir, "allowed"),
		},
		AgentBranch: "agent/test-abc",
	})
	err := m.Start()
	require.Error(t, err, "a file:// config origin outside local_origin_root must be rejected")
	require.Contains(t, err.Error(), "outside the allowed root")
}
