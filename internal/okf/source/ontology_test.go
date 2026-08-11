package source

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A committed ontology is read AT THE SOURCE COMMIT, which is what keeps the
// bundle a pure function of that commit rather than of the machine's state.
const testOntologyYAML = `id: source-code
name: Source Code Knowledge
description: Knowledge categories for AI agents working in a codebase.
topics:
  decisions:
    description: Design choices with rationale
    children:
      okf:
        description: The OKF export surface
`

// legacyOntologyYAML is testOntologyYAML with a different name:, so a test
// committing both paths can tell which one was actually read.
const legacyOntologyYAML = `id: source-code
name: Legacy Source Code Knowledge
description: Knowledge categories for AI agents working in a codebase.
topics:
  decisions:
    description: Design choices with rationale
    children:
      okf:
        description: The OKF export surface
`

// TestOntology_LegacyCommittedFileWins covers the read fallback for repos that
// predate the move into .knomit/: no migration is provided, so the legacy
// .domains/ path must still be honoured when it is the only one committed.
func TestOntology_LegacyCommittedFileWins(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		".domains/ontology.yaml":     testOntologyYAML,
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Empty(t, snap.Warnings)
	require.Equal(t, "Source Code Knowledge", snap.Ontology.Name)
	require.Equal(t, "Knowledge categories for AI agents working in a codebase.", snap.Ontology.Description)
	// Descriptions are flattened to paths relative to kb/, which is the shape
	// of the bundle's directory tree.
	require.Equal(t, "Design choices with rationale", snap.Ontology.Nodes["decisions"])
	require.Equal(t, "The OKF export surface", snap.Ontology.Nodes["decisions/okf"])
}

// The canonical private path (.knomit/) is preferred over the legacy one
// (.domains/).
//
// This also exercises Task 5's interaction: both paths are private, and the
// exporter must still read the canonical one by name while skipping it during
// the fact walk. If Load returned the default ontology here, the private
// check would have been wrongly applied to the ontology read.
func TestOntology_DotKnomitWinsOverLegacy(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		".knomit/ontology.yaml":      testOntologyYAML,
		".domains/ontology.yaml":     legacyOntologyYAML,
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Empty(t, snap.Warnings)
	require.Equal(t, "Source Code Knowledge", snap.Ontology.Name,
		".knomit/ must win when both are present")
}

// Absent ⇒ the embedded default, which is what the repo is actually validated
// against. A bundle without authored descriptions is still fully conformant.
func TestOntology_AbsentFallsBackToEmbeddedDefault(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Empty(t, snap.Warnings)
	require.Equal(t, "General Knowledge", snap.Ontology.Name,
		"a repo with no committed ontology falls back to the embedded default")
}

// Unparseable ⇒ the default plus a WARNING. It must not fail the export: the
// bundle is conformant without descriptions, and a broken ontology should not
// cost a publisher their whole knowledge base.
func TestOntology_UnparseableDegradesWithWarning(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		".domains/ontology.yaml":     "id: [this is not: valid yaml\n  at all",
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err, "a broken ontology must not fail the export")
	require.Equal(t, "General Knowledge", snap.Ontology.Name)
	require.Len(t, snap.Warnings, 1)
	require.Contains(t, snap.Warnings[0], "ontology parse:")
}

// TestLoad_ReadsTreeNotIndex pins determinism: enumeration is a pure function
// of the source SHA. A snapshot taken at an earlier commit must not include a
// fact added in a later one.
func TestLoad_ReadsTreeNotIndex(t *testing.T) {
	r := newFixtureRepo(t)
	early := commitWith(t, r, "learn: seed scope", "a+learn@agents.knomit.io", baseTime,
		map[string]string{"kb/decisions/x/aaaaaaaa.md": factBody("Scope", 0.9)})
	commitWith(t, r, "learn: seed later", "a+learn@agents.knomit.io", baseTime.Add(time.Minute),
		map[string]string{
			"kb/decisions/x/aaaaaaaa.md": factBody("Scope", 0.9),
			"kb/decisions/x/bbbbbbbb.md": factBody("Later", 0.9),
		}, early)

	snap, err := Load(r.Storer, early)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1, "snapshot at the earlier commit sees only the first fact")
	require.Equal(t, "kb/decisions/x/aaaaaaaa.md", snap.Facts[0].Fact.Path())
}
