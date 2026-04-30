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
	ri := m.Get("knomit")
	require.NotNil(t, ri)

	result, err := ri.svc.Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
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
		},
		AgentBranch: "agent/test-abc",
	})
	err := m.Start()
	require.NoError(t, err)
	ri := m.Get("knomit")
	require.NotNil(t, ri)

	result, err := ri.svc.Facts().ReadFact(context.Background(), "agent/test-abc", "domains/ontology.yaml", nil)
	require.NoError(t, err, "ontology must be readable from agent branch after init from empty remote")
	require.NotEmpty(t, result.Content, "ontology file must have content on agent branch")
}
