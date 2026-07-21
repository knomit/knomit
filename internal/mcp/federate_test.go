package mcp

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

func TestFuseRRF(t *testing.T) {
	// N=1: identity order (the RFC's N=1 no-behavior-change invariant).
	got := fuseRRF([]int{3})
	require.Equal(t, []mountRef{{0, 0}, {0, 1}, {0, 2}}, got)

	// Two lists [3,2]: rank layers, mount order within a layer.
	got = fuseRRF([]int{3, 2})
	require.Equal(t, []mountRef{
		{0, 0}, {1, 0}, // rank 0 layer
		{0, 1}, {1, 1}, // rank 1 layer
		{0, 2}, // rank 2 layer
	}, got)

	// Empty lists mixed with non-empty.
	got = fuseRRF([]int{0, 2, 0})
	require.Equal(t, []mountRef{{1, 0}, {1, 1}}, got)

	// All-empty → empty.
	require.Empty(t, fuseRRF([]int{0, 0}))
	require.Empty(t, fuseRRF(nil))
}

func TestMergeRecent(t *testing.T) {
	// [[100,50],[70]] → committed_at DESC: 100(0,0), 70(1,0), 50(0,1).
	got := mergeRecent([][]int64{{100, 50}, {70}}, 10)
	require.Equal(t, []mountRef{{0, 0}, {1, 0}, {0, 1}}, got)

	// cap max=2 truncates.
	got = mergeRecent([][]int64{{100, 50}, {70}}, 2)
	require.Equal(t, []mountRef{{0, 0}, {1, 0}}, got)

	// Equal stamps across mounts → mount order, then per-mount order.
	got = mergeRecent([][]int64{{100, 100}, {100}}, 10)
	require.Equal(t, []mountRef{{0, 0}, {0, 1}, {1, 0}}, got)
}

func TestID12(t *testing.T) {
	require.Equal(t, "3f9a2c1e8b7d", id12("3f9a2c1e8b7d0000000000000000000000000000"))
	// Short input returned as-is.
	require.Equal(t, "abc", id12("abc"))
	require.Equal(t, "3f9a2c1e8b7d", id12("3f9a2c1e8b7d"))
}

func TestQualifyPath(t *testing.T) {
	require.Equal(t, "kb://3f9a2c1e8b7d/kb/a/b.md", qualifyPath("3f9a2c1e8b7d", "kb/a/b.md"))
}

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
	if id12(large.ID()) < id12(small.ID()) {
		small, large = large, small
	}

	// Set A: ONE mount whose branch smuggles the separators — "a,<large-id>@b".
	craftedBranch := "a," + id12(large.ID()) + "@b"
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
			parts[i] = id12(rt.RI.ID()) + "@" + rt.Branch
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	require.Equal(t, oldFP(bA), oldFP(bB),
		"sanity: the two distinct read sets DID collide under the old scheme")

	require.NotEqual(t, readSetFingerprint(bA), readSetFingerprint(bB),
		"length-prefixed fingerprint must distinguish the two read sets (lenses RFC §7.3)")
}

func TestParseQualifiedPath(t *testing.T) {
	// Bare path → not qualified.
	id, rel, qualified, err := parseQualifiedPath("kb/a/b.md")
	require.NoError(t, err)
	require.False(t, qualified)
	require.Equal(t, "", id)
	require.Equal(t, "kb/a/b.md", rel)

	// Well-formed qualified path.
	id, rel, qualified, err = parseQualifiedPath("kb://3f9a2c1e8b7d/kb/a/b.md")
	require.NoError(t, err)
	require.True(t, qualified)
	require.Equal(t, "3f9a2c1e8b7d", id)
	require.Equal(t, "kb/a/b.md", rel)

	// Malformed variants.
	for _, p := range []string{
		"kb://short/x.md",
		"kb://GGGGGGGGGGGG/x.md",
		"kb://3f9a2c1e8b7d",  // no rel
		"kb://3f9a2c1e8b7d/", // empty rel
	} {
		_, _, qualified, err := parseQualifiedPath(p)
		require.True(t, qualified, "%q is a kb:// path", p)
		require.Error(t, err, "%q must be rejected", p)
	}
}

func TestWriteRepoPath(t *testing.T) {
	repoA := newLearnTestRepo(t, ontologyWithTopic(t, "decisions"))
	repoB := newLearnTestRepo(t, ontologyWithTopic(t, "other"))
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	writeID := id12(repoA.ID())
	readID := id12(repoB.ID())

	// Bare path → repo-relative, untouched.
	rel, err := writeRepoPath(b, "kb/a/b.md")
	require.NoError(t, err)
	require.Equal(t, "kb/a/b.md", rel)

	// Qualified to the write repo ≡ bare (RFC §6.2).
	rel, err = writeRepoPath(b, qualifyPath(writeID, "kb/a/b.md"))
	require.NoError(t, err)
	require.Equal(t, "kb/a/b.md", rel)

	// Qualified to a read mount → read-only-mount error naming the 12-hex id.
	_, err = writeRepoPath(b, qualifyPath(readID, "kb/a/b.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "read-only mount")
	require.Contains(t, err.Error(), readID)

	// Qualified to an unmounted ID → the shared not-mounted wording.
	_, err = writeRepoPath(b, "kb://ffffffffffff/kb/a/b.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not mounted in this binding")

	// Malformed kb:// path → parseQualifiedPath's error propagates.
	_, err = writeRepoPath(b, "kb://short/x.md")
	require.Error(t, err)
}

func TestTopicOfPathFilter(t *testing.T) {
	require.Equal(t, "decisions", topicOfPathFilter("kb/decisions/lens/"))
	require.Equal(t, "decisions", topicOfPathFilter("kb/decisions/x.md"))
	// Prefix filter, topic segment NOT delimited → no constraint.
	require.Equal(t, "", topicOfPathFilter("kb/decisions"))
	require.Equal(t, "", topicOfPathFilter("kb/"))
	require.Equal(t, "", topicOfPathFilter("kb"))
	require.Equal(t, "", topicOfPathFilter(""))
	require.Equal(t, "", topicOfPathFilter("other/x"))
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
	got, err := readTargetsFor(b, "kb/")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Same(t, repoA, got[0].RT.RI)
	require.Same(t, repoB, got[1].RT.RI)
	require.Equal(t, "kb/", got[0].Path)
	require.Equal(t, "kb/", got[1].Path)

	// Qualified filter → single mount, Path rewritten repo-relative.
	qual := qualifyPath(id12(repoB.ID()), "kb/x.md")
	got, err = readTargetsFor(b, qual)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Same(t, repoB, got[0].RT.RI)
	require.Equal(t, "kb/x.md", got[0].Path)

	// Qualified to an unmounted ID → error containing "not mounted".
	_, err = readTargetsFor(b, qualifyPath("aaaaaaaaaaaa", "kb/x.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not mounted")

	// Topic-constrained filter skips a mount whose Ontology lacks the topic.
	got, err = readTargetsFor(b, "kb/decisions/")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Same(t, repoA, got[0].RT.RI)

	// Malformed qualified filter → error.
	_, err = readTargetsFor(b, "kb://short/x.md")
	require.Error(t, err)
}
