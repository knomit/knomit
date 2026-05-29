package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
)

// TestLoadOntology_fallsBackToDefault verifies that a repo with no
// domains/ontology.yaml in git still gets a non-nil default ontology.
func TestLoadOntology_fallsBackToDefault(t *testing.T) {
	b := &repoBuilder{
		name:        "test",
		agentBranch: "machine/test",
	}
	// svc is nil; loadOntology must not panic and must return the default ontology.
	b.loadOntology()
	require.NotNil(t, b.ontology)
}

// bootKnomitWithStaleOntology starts a Manager once to bootstrap the default
// repo, then overwrites domains/ontology.yaml on the agent branch with the
// provided YAML, then closes the manager. The returned dir + agent branch
// can be passed to a second New(...)/Start() pair to exercise loadOntology
// against a stored ontology that lags the embedded preset.
func bootKnomitWithStaleOntology(t *testing.T, staleYAML string) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())

	ri := m.Get("knomit")
	require.NotNil(t, ri)

	// Overwrite the seeded ontology with the stale version on the agent branch.
	_, err := ri.svc.Facts().WriteFact(
		context.Background(),
		agentBranch,
		"domains/ontology.yaml",
		staleYAML,
		"test: seed stale ontology",
		"updated",
	)
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// TestLoadOntology_RefreshesPresetDerivedSubset seeds a repo with a stored
// ontology that is a strict subset of the current CodeOntology embedded
// preset, then re-opens the repo. loadOntology must detect the lag and
// rewrite domains/ontology.yaml with the latest preset, and b.ontology must
// reflect the upgraded version.
func TestLoadOntology_RefreshesPresetDerivedSubset(t *testing.T) {
	// A minimal source-code ontology: same id as CodeOntology but only one
	// topic. CodeOntology has 8 topics, so this is a strict subset.
	const staleYAML = `id: source-code
name: Source Code Knowledge
description: stale
topics:
  invariants:
    description: Load-bearing rules
`
	dir, agentBranch := bootKnomitWithStaleOntology(t, staleYAML)

	// Reopen against the same on-disk state — loadOntology should now run
	// the refresh path and rewrite the stored ontology to match CodeOntology.
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get("knomit")
	require.NotNil(t, ri)

	// b.ontology in memory must be the upgraded preset.
	require.NotNil(t, ri.Ontology())
	require.Equal(t, "source-code", ri.Ontology().ID)
	require.Contains(t, ri.Ontology().Topics, "principles",
		"refreshed ontology must include the principles topic from the latest preset")

	// The on-branch file must match CodeOntology().Serialize().
	result, err := ri.svc.Facts().ReadFact(context.Background(), agentBranch, "domains/ontology.yaml", nil)
	require.NoError(t, err)

	expectedY, err := fact.CodeOntology().Serialize()
	require.NoError(t, err)
	require.Equal(t, string(expectedY), result.Content,
		"domains/ontology.yaml on the agent branch must be rewritten to the embedded CodeOntology preset")
}

// TestLoadOntology_PreservesDivergedOntology seeds a repo with an ontology
// that shares the source-code id but adds a custom topic the preset lacks.
// loadOntology must NOT refresh — the user has diverged, and overwriting
// their custom topic would lose work.
func TestLoadOntology_PreservesDivergedOntology(t *testing.T) {
	// A diverged source-code ontology: same id, but adds a topic that is
	// NOT in CodeOntology. IsSubsetOf must return false; the refresh path
	// must be skipped.
	const divergedYAML = `id: source-code
name: Source Code Knowledge
description: user-customised
topics:
  invariants:
    description: Load-bearing rules
  my-custom-topic:
    description: A user-only topic not in any embedded preset
`
	dir, agentBranch := bootKnomitWithStaleOntology(t, divergedYAML)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get("knomit")
	require.NotNil(t, ri)

	// b.ontology must be the stored (diverged) ontology, NOT the preset.
	require.NotNil(t, ri.Ontology())
	require.Equal(t, "source-code", ri.Ontology().ID)
	require.Contains(t, ri.Ontology().Topics, "my-custom-topic",
		"diverged ontology must be preserved; custom topic must still be present in memory")
	require.NotContains(t, ri.Ontology().Topics, "principles",
		"diverged ontology must NOT be refreshed; preset-only topics must NOT appear")

	// The on-branch file must still match the diverged YAML.
	result, err := ri.svc.Facts().ReadFact(context.Background(), agentBranch, "domains/ontology.yaml", nil)
	require.NoError(t, err)
	require.Equal(t, divergedYAML, result.Content,
		"domains/ontology.yaml on the agent branch must NOT be rewritten when the stored ontology has diverged from the preset")
}
