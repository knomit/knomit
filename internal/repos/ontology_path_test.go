package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestOntologyPathsAreCanonicalAndLegacy pins the two ontology locations and
// their single source of truth: repos.OntologyPath/LegacyOntologyPath are not
// redeclared, they are the fact package's constants under a name every
// existing caller already uses. okf/source used to carry its own duplicated
// copies of these literals — this test is what would catch that drift coming
// back.
func TestOntologyPathsAreCanonicalAndLegacy(t *testing.T) {
	require.Equal(t, ".knomit/ontology.yaml", OntologyPath)
	require.Equal(t, ".domains/ontology.yaml", LegacyOntologyPath)
	require.True(t, strings.HasPrefix(OntologyPath, fact.PrivateRoot+"/"),
		"the canonical ontology must live in knomit's own namespace")
	// One definition, not three. okf/source used to carry its own copies.
	require.Equal(t, fact.OntologyFile, OntologyPath)
	require.Equal(t, fact.LegacyOntologyFile, LegacyOntologyPath)
}

// TestServerOwnedPathsAreNotAgentWritable guards against adding a new
// server-owned constant and forgetting to keep it out of the agent-writable
// surface: OntologyPath, LegacyOntologyPath, ReadmePath and LicensePath must
// never be reachable through the fact tools' write guard.
func TestServerOwnedPathsAreNotAgentWritable(t *testing.T) {
	for _, p := range []string{OntologyPath, LegacyOntologyPath, ReadmePath, LicensePath} {
		require.Falsef(t, fact.IsWritablePrivatePath(p),
			"%s is server-owned and must not be writable through the fact tools", p)
	}
}

// TestLoadOntology_ReadsCanonicalPath seeds ONLY the canonical path with a
// distinguishable, non-preset-derived ontology (so the boot-time refresh
// cannot fire and mask a bug in the read itself) and asserts the repo loads
// exactly that ontology rather than falling through to fact.DefaultOntology.
func TestLoadOntology_ReadsCanonicalPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, canonicalWinsYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Equal(t, "canonical-wins", ri.Ontology().ID,
		"the seeded canonical ontology must be loaded, not the default")
	require.NotEqual(t, fact.DefaultOntology().ID, ri.Ontology().ID)
}

// TestLoadOntology_FallsBackToLegacyPath seeds ONLY the legacy .domains/
// path — an unmigrated repo the user has not hand-moved yet — and asserts the
// repo still validates against ITS OWN taxonomy. Silently switching to the
// default here is exactly the failure this fallback exists to prevent: new
// facts would start validating against the wrong ontology with nothing in the
// logs tying them to the cause.
func TestLoadOntology_FallsBackToLegacyPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"the legacy ontology must be honoured, not replaced by the default")
}

// TestLoadOntology_PresetRefreshWritesBackToThePathItRead is THE regression
// guard for this task. The repo holds ONLY a legacy-path ontology that is a
// strict subset of the embedded preset, so the boot-time refresh in
// loadOntology fires and rewrites it. If that write went to the canonical
// path instead of srcPath, a legacy repo would end up holding TWO ontology
// files, with nothing distinguishing the stale one from the live one.
func TestLoadOntology_PresetRefreshWritesBackToThePathItRead(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		// The refresh must have fired: staleCodeOntologyYAML has one topic
		// ("invariants"); the embedded preset has eight, including
		// "incidents". Seeing "incidents" on the legacy path proves the
		// refresh ran, not just that the read succeeded.
		legacy, rerr := svc.Facts().ReadFact(context.Background(), agentBranch, LegacyOntologyPath, nil)
		require.NoError(t, rerr)
		require.Contains(t, legacy.Content, "incidents",
			"the refresh must have fired and landed on the path it read from")

		exists, eerr := svc.Facts().FactExists(context.Background(), agentBranch, OntologyPath)
		require.NoError(t, eerr)
		require.False(t, exists,
			"a legacy repo must not grow a second, canonical-path ontology file")
	}))
}
