package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
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
// Body is identical to the original with ontologyPath substituted for the
// hard-coded "domains/ontology.yaml" argument to WriteFact.
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

// canonicalWinsYAML and legacyLosesYAML seed the two competing files in
// TestLoadOntology_PrefersDotDomains with DIFFERENT ids — neither matching an
// embedded preset — so (a) the assertion can tell which file was actually
// read, mirroring the discrimination TestOntology_DotDomainsWinsOverLegacy
// (internal/okf/source/ontology_test.go) applies to the exporter's own
// ontology reader, and (b) EmbeddedPresetByID returns nil for both, keeping
// the boot-time refresh (which rewrites srcPath when the stored ontology is a
// preset subset) out of play — this test is purely about read preference.
const canonicalWinsYAML = `id: canonical-wins
name: Canonical Ontology
description: seeded at .domains/ontology.yaml
topics:
  invariants:
    description: Load-bearing rules
`

const legacyLosesYAML = `id: legacy-loses
name: Legacy Ontology
description: seeded at domains/ontology.yaml
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
func TestLoadOntology_PrefersDotDomains(t *testing.T) {
	dir, agentBranch := bootKnomitWithBothOntologies(t, canonicalWinsYAML, legacyLosesYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	ri := reopenTestRepo(t, m, dir)
	require.Equal(t, "canonical-wins", ri.Ontology().ID,
		"the canonical .domains/ontology.yaml must win when a legacy domains/ontology.yaml also exists")
}

// No migration is provided, so an unmigrated repo must keep validating
// against ITS ontology. Falling through to DefaultOntology would silently
// start accepting facts under topics this repo does not have.
func TestLoadOntology_FallsBackToLegacyDomains(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	ri := reopenTestRepo(t, m, dir)
	require.Equal(t, "source-code", ri.Ontology().ID,
		"the legacy ontology must be honoured, not replaced by the default")
}

// THE regression guard for this task. The stored ontology is a strict subset
// of the embedded preset, so the boot-time refresh fires and rewrites it. If
// that write went to the canonical path, a legacy repo would end up holding
// TWO ontology files, the stale one indistinguishable from the live one.
func TestLoadOntology_RefreshWritesBackToLegacyPath(t *testing.T) {
	dir, agentBranch := bootKnomitWithLegacyOnlyOntology(t, staleCodeOntologyYAML)

	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: dir}, AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	ri := reopenTestRepo(t, m, dir)

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		legacy, rerr := svc.Facts().ReadFact(context.Background(), agentBranch, LegacyOntologyPath, nil)
		require.NoError(t, rerr)
		require.Contains(t, legacy.Content, "incidents",
			"the refresh must land on the path it read from")

		exists, eerr := svc.Facts().FactExists(context.Background(), agentBranch, OntologyPath)
		require.NoError(t, eerr)
		require.False(t, exists,
			"a legacy repo must not grow a second ontology file")
	}))
}

// TestLoadOntology_RefreshesPresetDerivedSubset seeds a repo with a stored
// ontology that is a strict subset of the current CodeOntology embedded
// preset, then re-opens the repo. loadOntology must detect the lag and
// rewrite domains/ontology.yaml with the latest preset, and b.ontology must
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

	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	ri := reopenTestRepo(t, m, dir)

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

	// Start only opens what the registry says exists, and Create does not
	// register a row until Task 6 — so the reboot must re-open the repo by hand.
	ri := reopenTestRepo(t, m, dir)

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
