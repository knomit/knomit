package mcp

import (
	"strings"
	"testing"

	"knomit/internal/fact"

	"github.com/stretchr/testify/require"
)

// TestValidateAndBuildFacts_UppercaseOntologyRoot pins where a newly-minted
// fact is actually committed.
//
// fact.BuildFactPath passes the configured ontology root through verbatim and
// lowercases only topic/category, while fact.NewFact lowercases the ENTIRE
// path it is handed — root included. config.Validate accepts an
// ontology_root of "KB", so the two disagree exactly when the root is not
// already lowercase. Keying the pending-write map off f.Path() therefore
// commits the file to "kb/…" while the ontology lives at "KB/…", putting
// every new fact outside the root where ontology-scoped queries cannot see it.
//
// The invariant: the files map is keyed by ON-DISK path, which always carries
// the configured root's real case. f.Path() is a normalized identity and is
// never a location.
func TestValidateAndBuildFacts_UppercaseOntologyRoot(t *testing.T) {
	inputs := []learnFactInput{{
		Topic:      "technology",
		Category:   "languages/go",
		Title:      "T",
		Body:       "B",
		Confidence: 0.8,
		Sources:    1,
	}}

	facts, _, paths, files, err := validateAndBuildFacts(nil, "KB", inputs)
	require.NoError(t, err)
	require.Len(t, files, 1)

	for key := range files {
		require.Truef(t, strings.HasPrefix(key, "KB/"),
			"fact committed to %q, which is outside the configured ontology root %q — "+
				"ontology-scoped queries will never find it", key, "KB")
	}
	// paths is the authoritative location and must agree with the map key.
	require.Contains(t, files, paths[0])
	// And the identity is legitimately the lowercased form — this is what
	// makes the two non-interchangeable, so the test asserts the divergence
	// rather than pretending it does not exist.
	require.Equal(t, strings.ToLower(paths[0]), facts[0].Path())
}

// TestMergeFacts_LineageRefUsesRawPath pins that the lineage ref a dedup-merge
// appends points at a file that exists on disk.
//
// The merged fact's IDENTITY is the normalized (lowercased) path — that is
// what identity means, and ToLower is idempotent. But the REF is a pointer to
// a file. `existing` is produced by ParseFact(rawPath, …), whose last step
// lowercases the whole path, so existing.Path() for "kb/Tech/Foo.md" is
// "kb/tech/foo.md" — a path with no file behind it. Using it as the lineage
// ref makes every provenance walk through that edge dangle.
func TestMergeFacts_LineageRefUsesRawPath(t *testing.T) {
	const rawPath = "kb/Tech/Foo.md"

	existing := fact.NewFact(rawPath) // mirrors ParseFact: lowercases
	existing.Title, existing.Body, existing.Type = "E", "eb", fact.Observation
	existing.Confidence, existing.Sources = 0.9, 2
	existing.Domain, existing.Entities, existing.Refs = []string{"d"}, []string{}, []string{}

	incoming := fact.NewFact("kb/tech/new.md")
	incoming.Title, incoming.Body, incoming.Type = "N", "nb", fact.Observation
	incoming.Confidence, incoming.Sources = 0.5, 1
	incoming.Domain, incoming.Entities, incoming.Refs = []string{"d"}, []string{}, []string{}

	merged := mergeFacts(incoming, existing, rawPath)

	require.Contains(t, merged.Refs, rawPath,
		"lineage ref must be the raw on-disk path so provenance walks resolve")
	require.NotContains(t, merged.Refs, strings.ToLower(rawPath),
		"lowercased path names no file on disk; a ref to it dangles")
	// Identity stays normalized — unchanged from the pre-refactor behaviour.
	require.Equal(t, strings.ToLower(rawPath), merged.Path())
}
