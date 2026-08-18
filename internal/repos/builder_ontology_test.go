package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
)

// A builder with no store establishes NOTHING about a repo's taxonomy, so it
// gets no ontology — and, as before, does not panic.
//
// This test used to assert the opposite: that the default ontology was handed
// out here. That fallback was the mechanism behind the worst failure this
// codebase has had — a repo silently running on a taxonomy nobody chose, with
// every fact validated against the wrong topics and no way back, because a
// repo's ontology is fixed when it is created. ALL REPOS MUST HAVE AN
// ONTOLOGY; none may be given one it did not choose.
func TestLoadOntology_NoStoreEstablishesNothing(t *testing.T) {
	b := &repoBuilder{
		name:        "test",
		agentBranch: "machine/test",
	}
	b.loadOntology()
	require.Nil(t, b.ontology, "a stand-in ontology is not this repo's ontology")
	require.Error(t, b.ontologyErr)
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

// TestLoadOntology_UnknownKeyDoesNotSubstituteTheDefault is the open-path
// regression for the strict unknown-key check.
//
// A repo's ontology is fixed at create time and is never user-editable
// afterwards, so the open path has exactly one correct behaviour: read what is
// committed. When ValidateOntologyYAML began reporting unknown keys, and
// ParseOntology turned any diagnostic into a hard failure, loadOntology's
// answer to a parse failure — one log.Warn, then fact.DefaultOntology() —
// turned every such ontology into "General" on the next open. The user picked
// Code and got General, permanently, with nothing in the UI saying so: exactly
// the failure the rest of this work exists to close, arriving through the open
// door instead of the create door.
//
// A key this binary does not declare is not a broken ontology. It is a repo
// written by a NEWER knomit, or hand-edited in git — which is the product's own
// model of how a knowledge base is worked on. Rejecting it also made every
// future field addition a silent downgrade for anyone on an older build.
func TestLoadOntology_UnknownKeyDoesNotSubstituteTheDefault(t *testing.T) {
	// `version:` is not a field of fact.Ontology. Everything else here is
	// valid, and the taxonomy below is what this repo must keep.
	const futureYAML = `id: source-code
name: Source Code Knowledge
description: written by a newer knomit
version: 2
topics:
  invariants:
    description: Load-bearing rules
`
	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, futureYAML)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.NotNil(t, ri.Ontology())

	require.Equal(t, "source-code", ri.Ontology().ID,
		"the repo opened with a DIFFERENT ontology than the one committed to its agent branch")
	require.NotEqual(t, fact.DefaultOntology().ID, ri.Ontology().ID,
		"an unknown key must not silently replace the repo's taxonomy with the default one")
	require.Contains(t, ri.Ontology().Topics, "invariants")
}

// ALL REPOS MUST HAVE AN ONTOLOGY.
//
// A repo whose ontology cannot be established does NOT get a substitute. It
// used to get fact.DefaultOntology() behind one log line, which meant it came
// up looking healthy while running on a taxonomy nobody chose — and every fact
// written afterwards was validated against the wrong topics, permanently,
// because the ontology is fixed at create time and never editable.
//
// The repo still opens: refusing to open would strand the user's data behind an
// error they cannot inspect. What it does not do is accept writes. Reads work,
// the error is reported, and nothing can be authored against a taxonomy that
// was never chosen.
func TestLoadOntology_MalformedOntologyRefusesWritesRatherThanSubstituting(t *testing.T) {
	// Valid YAML that is not an ontology: `id` is required, so this cannot
	// parse into one, and no fallback may cover for it.
	const brokenYAML = "topics:\n  invariants:\n    description: Load-bearing rules\n"

	dir, agentBranch := bootKnomitWithStaleOntologyAt(t, OntologyPath, brokenYAML)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri, "the repo must still open — its data has to stay reachable")

	require.Error(t, ri.OntologyError(), "the failure must be reported, not absorbed")
	require.Nil(t, ri.Ontology(),
		"a repo with no established ontology must not present one; the default is not this repo's taxonomy")
	require.False(t, ri.WritableBranch(agentBranch),
		"no branch is writable while the ontology is unestablished — every write would be validated against the wrong topics")
}

// The same rule for a repo carrying no ontology file at all. Being an ordinary
// git repository is not being a knowledge base.
func TestLoadOntology_NoOntologyFileRefusesWrites(t *testing.T) {
	dir, agentBranch := bootKnomitWithNoOntology(t)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	require.Error(t, ri.OntologyError())
	require.Nil(t, ri.Ontology())
	require.False(t, ri.WritableBranch(agentBranch))
}

// bootKnomitWithNoOntology removes BOTH ontology paths, leaving a repo that is
// an ordinary git repository and nothing more.
func bootKnomitWithNoOntology(t *testing.T) (dir, agentBranch string) {
	t.Helper()
	dir = t.TempDir()
	agentBranch = "agent/test-stale"

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	ri := bootRepo(t, m)
	require.NotNil(t, ri)

	_, err := testService(t, ri).Facts().DeleteFact(
		context.Background(), agentBranch, OntologyPath, "test: remove the ontology")
	require.NoError(t, err)

	require.NoError(t, m.Close())
	return dir, agentBranch
}

// A LENS MUST NOT BE A WAY IN.
//
// NewBindingOfLens hardcoded writeOK:true, reasoning that lens writes always
// target the write repo's own agent branch — true, and exactly why it has to
// ask the same question everything else asks. A repo with no established
// ontology accepts no writes through any door; a lens that wrote to it would
// author facts against topics nobody chose, which is the whole failure.
func TestBindingOfLens_RefusesWritesToARepoWithNoOntology(t *testing.T) {
	dir, agentBranch := bootKnomitWithNoOntology(t)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: agentBranch,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	b, err := NewBindingOfLens(m, Lens{
		Name: "l", UID: "lens-uid", WriteUID: ri.UID(),
		Reads: []LensRead{{RepoUID: ri.UID()}},
	})
	require.NoError(t, err, "the lens still RESOLVES — it is writing that is refused, not looking")
	require.False(t, b.WriteOK(),
		"a lens must not be a way to write into a repo whose ontology is unestablished")
}
