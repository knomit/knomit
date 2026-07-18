package mcp

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
)

// TestReadSetFingerprint_InjectiveAcrossBranchSeparators pins the fix for the
// collision the old "id12@branch, comma-joined" scheme allowed: because '@' and
// ',' also appear inside branch names, a single mount at branch "a,<id2>@b"
// serialized byte-identically to two separate mounts "<id1>@a" + "<id2>@b",
// letting a stale cursor survive a lens redefinition (lenses RFC §7.3). The
// length-prefixed encoding must keep these two DISTINCT read sets distinct.
func TestReadSetFingerprint_InjectiveAcrossBranchSeparators(t *testing.T) {
	r1 := newLearnTestRepo(t, fact.CodeOntology())
	r2 := newLearnTestRepo(t, fact.CodeOntology())
	// Order so the single-mount repo (small id) sorts before the embedded one,
	// making the old-scheme sorted join of the two-mount set collide exactly.
	small, large := r1, r2
	if federate.ID12(large.ID()) < federate.ID12(small.ID()) {
		small, large = large, small
	}

	// Set A: ONE mount whose branch smuggles the separators — "a,<large-id>@b".
	craftedBranch := "a," + federate.ID12(large.ID()) + "@b"
	bA := repos.NewBindingForTest(small, repos.ReadTarget{RI: small, Branch: craftedBranch})
	// Set B: TWO genuinely-distinct mounts, small@a + large@b.
	bB := repos.NewBindingForTest(small,
		repos.ReadTarget{RI: small, Branch: "a"},
		repos.ReadTarget{RI: large, Branch: "b"},
	)

	// The old buggy encoding: id12@branch, sorted, comma-joined.
	oldFP := func(b *repos.Binding) string {
		reads := b.Reads()
		parts := make([]string, len(reads))
		for i, rt := range reads {
			parts[i] = federate.ID12(rt.RI.ID()) + "@" + rt.Branch
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	require.Equal(t, oldFP(bA), oldFP(bB),
		"sanity: the two distinct read sets DID collide under the old scheme")

	require.NotEqual(t, federate.ReadSetFingerprint(bA), federate.ReadSetFingerprint(bB),
		"length-prefixed fingerprint must distinguish the two read sets (lenses RFC §7.3)")
}

func TestWriteRepoPath(t *testing.T) {
	repoA := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	repoB := newLearnTestRepo(t, ontologyWithTopic(t, "other"))
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	writeID := federate.ID12(repoA.ID())
	readID := federate.ID12(repoB.ID())

	// Bare path → repo-relative, untouched.
	rel, err := federate.WriteRepoPath(b, "kb/a/b.md")
	require.NoError(t, err)
	require.Equal(t, "kb/a/b.md", rel)

	// Qualified to the write repo ≡ bare (RFC §6.2).
	rel, err = federate.WriteRepoPath(b, federate.QualifyPath(writeID, "kb/a/b.md"))
	require.NoError(t, err)
	require.Equal(t, "kb/a/b.md", rel)

	// Qualified to a read mount → read-only-mount error naming the 12-hex id.
	_, err = federate.WriteRepoPath(b, federate.QualifyPath(readID, "kb/a/b.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "read-only mount")
	require.Contains(t, err.Error(), readID)

	// Qualified to an unmounted ID → the shared not-mounted wording.
	_, err = federate.WriteRepoPath(b, "kb://ffffffffffff/kb/a/b.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not mounted in this binding")

	// Malformed kb:// path → ParseQualifiedPath's error propagates.
	_, err = federate.WriteRepoPath(b, "kb://short/x.md")
	require.Error(t, err)
}

// ontologyWithTopic parses a minimal one-topic ontology.
func ontologyWithTopic(t *testing.T, topic string) *fact.Ontology {
	t.Helper()
	o, err := fact.ParseOntology([]byte("id: t\nname: T\ntopics:\n  " + topic + ":\n    description: x\n"))
	require.NoError(t, err)
	return o
}

func TestReadTargetsFor(t *testing.T) {
	repoA := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	repoB := newLearnTestRepo(t, ontologyWithTopic(t, "other"))
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)

	// Unqualified filter with no topic → all read mounts, Path passed through.
	got, err := federate.ReadTargetsFor(b, "kb/")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Same(t, repoA, got[0].RT.RI)
	require.Same(t, repoB, got[1].RT.RI)
	require.Equal(t, "kb/", got[0].Path)
	require.Equal(t, "kb/", got[1].Path)

	// Qualified filter → single mount, Path rewritten repo-relative.
	qual := federate.QualifyPath(federate.ID12(repoB.ID()), "kb/x.md")
	got, err = federate.ReadTargetsFor(b, qual)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Same(t, repoB, got[0].RT.RI)
	require.Equal(t, "kb/x.md", got[0].Path)

	// Qualified to an unmounted ID → error containing "not mounted".
	_, err = federate.ReadTargetsFor(b, federate.QualifyPath("aaaaaaaaaaaa", "kb/x.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not mounted")

	// Topic-constrained filter skips a mount whose Ontology lacks the topic.
	got, err = federate.ReadTargetsFor(b, "kb/decisions/")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Same(t, repoA, got[0].RT.RI)

	// Malformed qualified filter → error.
	_, err = federate.ReadTargetsFor(b, "kb://short/x.md")
	require.Error(t, err)
}
