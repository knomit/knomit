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
		Confidence: ptr(0.8),
		Sources:    ptr(1),
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

// TestMergeFacts_AppendsNoSelfLineageRef replaces TestMergeFacts_LineageRefUsesRawPath,
// which pinned that the appended lineage ref used the RAW on-disk path so a
// provenance walk through it would not dangle. That whole concern is gone with
// the ref: #132 stopped the merge appending its own path at all, because the
// merge RETARGETS onto that path, so the "lineage" ref pointed at the merged
// fact itself.
//
// The mixed-case fixture is KEPT deliberately. Raw vs normalized was the reason
// the old ref existed, so it is the case most likely to be reintroduced by
// someone restoring the append "correctly" — a merge that emits EITHER spelling
// of its own path fails here.
func TestMergeFacts_AppendsNoSelfLineageRef(t *testing.T) {
	const rawPath = "kb/Tech/Foo.md"

	existing := fact.NewFact(rawPath) // mirrors ParseFact: lowercases
	existing.Title, existing.Body, existing.Type = "E", "eb", fact.Observation
	existing.Confidence, existing.Sources = 0.9, 2
	existing.Domain, existing.Entities, existing.Refs = []string{"d"}, []string{}, []string{}

	incoming := fact.NewFact("kb/tech/new.md")
	incoming.Title, incoming.Body, incoming.Type = "N", "nb", fact.Observation
	incoming.Confidence, incoming.Sources = 0.5, 1
	incoming.Domain, incoming.Entities, incoming.Refs = []string{"d"}, []string{}, []string{}

	merged := mergeFacts(incoming, existing, testLocalID)

	require.NotContains(t, merged.Refs, rawPath,
		"the merge must not append its own path as lineage (#132)")
	require.NotContains(t, merged.Refs, strings.ToLower(rawPath),
		"nor the normalized spelling of it")
	require.Empty(t, merged.Refs,
		"neither operand carried a ref, so the merge must produce none")
	// Identity stays normalized — unchanged.
	require.Equal(t, strings.ToLower(rawPath), merged.Path())
}

// TestMergeFacts_DropsARefThatBecomesSelfReferential is the SECOND way a merge
// used to emit a self-ref, and the one removing the append does not close: the
// incoming fact legitimately cites the fact it then merges INTO. Nothing is
// appended — the ref was already there, and the retarget is what changes its
// meaning from "B cites A" to "A cites A".
//
// Both stored spellings are exercised, because refs arrive bare from a caller
// and canonical from storage, and a filter that classified with an empty repo
// id would read the canonical one as foreign and keep it.
func TestMergeFacts_DropsARefThatBecomesSelfReferential(t *testing.T) {
	const existingPath = "kb/tech/foo.md"
	const otherPath = "kb/tech/other.md"

	for _, tc := range []struct {
		name    string
		selfRef string
	}{
		{"bare", existingPath},
		{"canonical", fact.QualifyKBPath(testLocalID, existingPath)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := fact.NewFact(existingPath)
			existing.Title, existing.Body, existing.Type = "E", "eb", fact.Observation
			existing.Confidence, existing.Sources = 0.9, 2
			existing.Domain, existing.Entities = []string{"d"}, []string{}
			existing.Refs = []string{}

			incoming := fact.NewFact("kb/tech/new.md")
			incoming.Title, incoming.Body, incoming.Type = "N", "nb", fact.Observation
			incoming.Confidence, incoming.Sources = 0.5, 1
			incoming.Domain, incoming.Entities = []string{"d"}, []string{}
			// Cites the fact it is about to be merged into, plus an unrelated one.
			incoming.Refs = []string{tc.selfRef, otherPath}

			merged := mergeFacts(incoming, existing, testLocalID)

			require.NotContains(t, merged.Refs, tc.selfRef,
				"a ref the retarget turned into a self-reference must be dropped")
			require.Contains(t, merged.Refs, otherPath,
				"refs to OTHER facts must survive — dropping those would destroy lineage")
		})
	}
}

// knomit_learn must refuse exactly what the REST create path refuses: the two
// share BuildFactPath, so they must share the rule that bounds its output.
func TestValidateAndBuildFacts_RejectsPrivateTopic(t *testing.T) {
	_, _, _, _, err := validateAndBuildFacts(nil, "kb", []learnFactInput{
		{Topic: ".secret", Category: "x", Title: "T", Body: "B"},
	})
	if err == nil {
		t.Fatal("a private topic must be rejected, not allocated")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("error should name the private-path rule, got %v", err)
	}
}

// A private CATEGORY is the same hazard one segment deeper.
func TestValidateAndBuildFacts_RejectsPrivateCategory(t *testing.T) {
	_, _, _, _, err := validateAndBuildFacts(nil, "kb", []learnFactInput{
		{Topic: "decisions", Category: ".wip", Title: "T", Body: "B"},
	})
	if err == nil {
		t.Fatal("a private category must be rejected, not allocated")
	}
}
