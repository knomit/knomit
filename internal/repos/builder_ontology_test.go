package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
)

// TestLoadOntology_fallsBackToDefault verifies that a repo with no ontology
// file in git still gets a non-nil default ontology.
func TestLoadOntology_fallsBackToDefault(t *testing.T) {
	b := &repoBuilder{
		name:        "test",
		agentBranch: "machine/test",
	}
	// svc is nil; loadOntology must not panic and must return the default ontology.
	b.loadOntology()
	require.NotNil(t, b.ontology)
}

// staleCodeOntologyYAML is a minimal source-code ontology: same id as
// CodeOntology but only one topic. CodeOntology has 8 topics (including
// "incidents", checked below), so this is a strict subset — shared by every
// test that needs a stored ontology lagging the embedded preset.
const staleCodeOntologyYAML = `id: source-code
name: Source Code Knowledge
description: stale
topics:
  invariants:
    description: Load-bearing rules
`

// bootKnomitWithStaleOntologyAt is bootKnomitWithStaleOntology with an explicit
// ontology path, so a test can seed the canonical or the legacy location.
// Body is identical to the original with ontologyPath substituted for a
// hard-coded path argument to WriteFact.
func bootKnomitWithStaleOntologyAt(t *testing.T, ontologyPath, staleYAML string) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	ri := bootRepo(t, m)
	require.NotNil(t, ri)

	// Overwrite the seeded ontology with the stale version on the agent branch.
	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(),
		agentBranch,
		ontologyPath,
		staleYAML,
		"test: seed stale ontology",
		"updated",
	)
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// bootKnomitWithLegacyOnlyOntology seeds the legacy path and REMOVES the
// canonical one, reproducing an unmigrated repo exactly. Same body as
// bootKnomitWithStaleOntologyAt(t, LegacyOntologyPath, staleYAML) plus, before
// m.Close(), deleting the canonical file that the initial boot seeded — a
// freshly-seeded repo always has a canonical ontology after Task 7's seed
// change, so a legacy-only fixture must remove it or the fallback path this
// helper exists to exercise is never actually hit.
func bootKnomitWithLegacyOnlyOntology(t *testing.T, staleYAML string) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	ri := bootRepo(t, m)
	require.NotNil(t, ri)

	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(),
		agentBranch,
		LegacyOntologyPath,
		staleYAML,
		"test: seed stale ontology",
		"updated",
	)
	require.NoError(t, err)

	_, err = testService(t, ri).Facts().DeleteFact(
		context.Background(), agentBranch, OntologyPath, "test: remove canonical ontology")
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// bootKnomitWithPreDotOnlyOntology reproduces the OLDEST unmigrated repo:
// only domains/ontology.yaml, no dot anywhere. .domains/ landed six days
// before .knomit/ did, so a repo that predates BOTH has had to be hand-moved
// twice in a week to be anywhere else — which is to say, most have not.
func bootKnomitWithPreDotOnlyOntology(t *testing.T, staleYAML string) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	ri := bootRepo(t, m)
	require.NotNil(t, ri)

	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(),
		agentBranch,
		PreDotOntologyPath,
		staleYAML,
		"test: seed stale ontology",
		"updated",
	)
	require.NoError(t, err)

	_, err = testService(t, ri).Facts().DeleteFact(
		context.Background(), agentBranch, OntologyPath, "test: remove canonical ontology")
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// canonicalWinsYAML and legacyLosesYAML seed the two competing files in
// TestLoadOntology_PrefersCanonicalOverLegacy with DIFFERENT ids — neither
// matching an embedded preset — so (a) the assertion can tell which file was
// actually read, mirroring the discrimination
// TestOntology_DotKnomitWinsOverLegacy (internal/okf/source/ontology_test.go)
// applies to the exporter's own ontology reader, and (b) EmbeddedPresetByID
// returns nil for both, keeping the boot-time refresh (which rewrites
// srcPath when the stored ontology is a preset subset) out of play — this
// test is purely about read preference.
const canonicalWinsYAML = `id: canonical-wins
name: Canonical Ontology
description: seeded at .knomit/ontology.yaml
topics:
  invariants:
    description: Load-bearing rules
`

const legacyLosesYAML = `id: legacy-loses
name: Legacy Ontology
description: seeded at .domains/ontology.yaml
topics:
  invariants:
    description: Load-bearing rules
`

// bootKnomitWithBothOntologies seeds distinguishable ontologies at BOTH the
// canonical and the legacy path, reproducing a repo that holds both (e.g.
// mid hand-migration) — the only fixture that actually exercises "wins when
// both exist"; bootKnomitWithStaleOntologyAt alone seeds just one path.
func bootKnomitWithBothOntologies(t *testing.T, canonicalYAML, legacyYAML string) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	ri := bootRepo(t, m)
	require.NotNil(t, ri)

	_, err := testService(t, ri).Facts().WriteFact(
		context.Background(), agentBranch, OntologyPath, canonicalYAML,
		"test: seed canonical ontology", "updated")
	require.NoError(t, err)

	_, err = testService(t, ri).Facts().WriteFact(
		context.Background(), agentBranch, LegacyOntologyPath, legacyYAML,
		"test: seed legacy ontology alongside canonical", "updated")
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// The canonical path wins when both exist. Without bootKnomitWithBothOntologies
// seeding the legacy file too, this test could not fail: there would be
// nothing else for the canonical to "win" over.
//
// The read-preference and fallback/refresh scenarios this test's siblings
// used to cover (legacy-only read, refresh write-back) now live in
// ontology_path_test.go, named after what they assert rather than after the
// path literals of a since-dropped intermediate layout.
func TestLoadOntology_PrefersCanonicalOverLegacy(t *testing.T) {
	dir, agentBranch := bootKnomitWithBothOntologies(t, canonicalWinsYAML, legacyLosesYAML)

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
		"the canonical .knomit/ontology.yaml must win when a legacy .domains/ontology.yaml also exists")
}

// TestLoadOntology_RefreshesPresetDerivedSubset seeds a repo with a stored
// ontology that is a strict subset of the current CodeOntology embedded
// preset, then re-opens the repo. loadOntology must detect the lag and
// rewrite the ontology file with the latest preset, and b.ontology must
// reflect the upgraded version.
func TestLoadOntology_RefreshesPresetDerivedSubset(t *testing.T) {
	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, staleCodeOntologyYAML)

	// Reopen against the same on-disk state — loadOntology should now run
	// the refresh path and rewrite the stored ontology to match CodeOntology.
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	// b.ontology in memory must be the upgraded preset.
	require.NotNil(t, ri.Ontology())
	require.Equal(t, "source-code", ri.Ontology().ID)
	require.Contains(t, ri.Ontology().Topics, "principles",
		"refreshed ontology must include the principles topic from the latest preset")

	// The on-branch file must match CodeOntology().Serialize().
	result, err := testService(t, ri).Facts().ReadFact(context.Background(), agentBranch, OntologyPath, nil)
	require.NoError(t, err)

	expectedY, err := fact.CodeOntology().Serialize()
	require.NoError(t, err)
	require.Equal(t, string(expectedY), result.Content,
		"the ontology on the agent branch must be rewritten to the embedded CodeOntology preset")
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
	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, divergedYAML)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start opens what the registry says exists — Create registered this repo,
	// so the reboot re-opens it on its own.
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	// b.ontology must be the stored (diverged) ontology, NOT the preset.
	require.NotNil(t, ri.Ontology())
	require.Equal(t, "source-code", ri.Ontology().ID)
	require.Contains(t, ri.Ontology().Topics, "my-custom-topic",
		"diverged ontology must be preserved; custom topic must still be present in memory")
	require.NotContains(t, ri.Ontology().Topics, "principles",
		"diverged ontology must NOT be refreshed; preset-only topics must NOT appear")

	// The on-branch file must still match the diverged YAML.
	result, err := testService(t, ri).Facts().ReadFact(context.Background(), agentBranch, OntologyPath, nil)
	require.NoError(t, err)
	require.Equal(t, divergedYAML, result.Content,
		"the ontology on the agent branch must NOT be rewritten when the stored ontology has diverged from the preset")
}
