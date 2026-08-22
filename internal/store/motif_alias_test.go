package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The mechanical half of alias resolution (blueprint §3.1 step 1): stemming +
// canonicalization, before any LLM is involved. Everything asserted here is
// deterministic and rebuildable from fact_motifs alone — that is what makes it
// derived state under MN3 rather than a second copy of the authored claim.

func TestMotifAliases_StemmedSpellingsShareACanonicalID(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	// Same mechanism, two spellings differing only by plural.
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallbacks")
	require.NoError(t, err)
	require.Equal(t, a, b, "plural and singular spellings must resolve together")
	require.Contains(t, []string{"silent-fallback", "silent-fallbacks"}, a,
		"the canonical id must be a real member spelling, not a synthetic token string — "+
			"it is shown in the backfill prompt and the explain surface")
}

func TestMotifAliases_TokenOrderIsIgnored(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"atomic-write"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"write-atomic"})

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "atomic-write")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "write-atomic")
	require.NoError(t, err)
	require.Equal(t, a, b, "the grouping key is a token MULTISET; word order must not split a cluster")
}

func TestMotifAliases_DistinctMechanismsStayApart(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "config-drift")
	require.NoError(t, err)
	require.NotEqual(t, a, b,
		"the mechanical layer must not merge unrelated mechanisms — over-merge is the "+
			"failure that makes the whole axis useless, and it is invisible from the outside")
}

// The representative spelling is the highest-df member: it is the one the
// corpus actually uses, so it is the one a reader recognises and the one the
// backfill prompt should offer.
//
// The fixture is chosen so df order and lexicographic order DISAGREE:
// "write-atomic" is the more frequent spelling but sorts AFTER "atomic-write".
// An earlier fixture used silent-fallback/silent-fallbacks, where the
// highest-df member happened to sort first too — so deleting the df tiebreak
// entirely left the test green. It was passing on the tiebreak, not on the
// rule.
func TestMotifAliases_CanonicalIDIsTheHighestDFSpelling(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"write-atomic"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"write-atomic"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"atomic-write"})

	id, err := svc.Motifs().CanonicalID(ctx, branch, "atomic-write")
	require.NoError(t, err)
	require.Equal(t, "atomic-write", id, "precondition: unresolved resolves to itself")

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	id, err = svc.Motifs().CanonicalID(ctx, branch, "atomic-write")
	require.NoError(t, err)
	require.Equal(t, "write-atomic", id,
		"the representative must be the most-used spelling, not the first alphabetically")
}

// An unknown motif resolves to ITSELF rather than erroring or returning empty.
// Every consumer (df, matching, surfaces) then treats a corpus with no alias
// table as one where each motif is its own singleton cluster — which is
// precisely the pre-alias behaviour, so nothing downstream needs a special
// case for "aliases not built yet".
func TestMotifAliases_UnknownMotifResolvesToItself(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})

	// Deliberately BEFORE any RebuildAliases call.
	id, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.Equal(t, "silent-fallback", id)
}

// MN3, in the shape the designer pre-approved: the alias table is derived
// state. Dropping it and rebuilding must reproduce the same canonical ids AND
// leave every fact's bytes untouched. The blob-hash half is the one that
// matters — it is the assertion that nothing in alias resolution ever wrote
// back into a fact's frontmatter.
func TestMotifAliases_MN3_RebuildIsReproducibleAndTouchesNoFact(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"config-drift"})

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	before, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.NotEmpty(t, before, "fixture must produce alias rows, or this test proves nothing")

	blobsBefore := map[string]string{}
	for _, p := range []string{"kb/alpha/one.md", "kb/alpha/two.md", "kb/alpha/three.md"} {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		require.NotNil(t, rec)
		blobsBefore[p] = rec.BlobHash
	}

	_, err = svc.si.rh.db.ExecContext(ctx, `DELETE FROM motif_aliases`)
	require.NoError(t, err)
	empty, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, empty, "the drop must actually empty the table, or the rebuild below proves nothing")

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	after, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, before, after, "rebuilding from the facts alone must reproduce the mapping exactly")

	for p, want := range blobsBefore {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		require.NotNil(t, rec)
		require.Equal(t, want, rec.BlobHash,
			"MN3: alias resolution must never rewrite a fact — %s changed", p)
	}
}

// Retracting a motif from the corpus must retire its alias row. Otherwise the
// vocabulary only ever grows, and df computed over a stale cluster counts
// members no live fact carries.
func TestMotifAliases_RebuildRetiresVanishedSpellings(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	tbl, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.Contains(t, tbl, "config-drift")

	// Rewrite two.md without its motif; the spelling is now carried by nothing.
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	tbl, err = svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.NotContains(t, tbl, "config-drift",
		"a spelling no live fact carries must not linger in the vocabulary")
}

// The designer's T2 rider, asserted at the layer that has to honour it: the
// representative spelling FLIPS as df shifts, and the cluster key does not.
//
// This is what lets a definition survive a usage shift. Key a definition on
// the representative and the flip below orphans it — while the cluster it
// describes has not changed at all.
func TestMotifAliases_ClusterKeyIsStableWhenTheRepresentativeFlips(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()

	// "atomic-write" leads on df.
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"atomic-write"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"atomic-write"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"write-atomic"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	repBefore, err := svc.Motifs().CanonicalID(ctx, branch, "write-atomic")
	require.NoError(t, err)
	keyBefore, err := svc.Motifs().ClusterKey(ctx, branch, "write-atomic")
	require.NoError(t, err)
	require.Equal(t, "atomic-write", repBefore)

	// Usage shifts: two more facts adopt the other spelling, so it now leads.
	writeMotifFact(t, svc, branch, "kb/alpha/four.md", []string{"write-atomic"})
	writeMotifFact(t, svc, branch, "kb/alpha/five.md", []string{"write-atomic"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	repAfter, err := svc.Motifs().CanonicalID(ctx, branch, "write-atomic")
	require.NoError(t, err)
	keyAfter, err := svc.Motifs().ClusterKey(ctx, branch, "write-atomic")
	require.NoError(t, err)

	require.NotEqual(t, repBefore, repAfter,
		"precondition: the fixture must actually flip the representative, or this proves nothing")
	require.Equal(t, "write-atomic", repAfter)
	require.Equal(t, keyBefore, keyAfter,
		"the cluster key must NOT move when the representative does — this is what "+
			"lets a definition outlive a usage shift")

	// And both spellings agree on the key, since they are one cluster.
	otherKey, err := svc.Motifs().ClusterKey(ctx, branch, "atomic-write")
	require.NoError(t, err)
	require.Equal(t, keyAfter, otherKey)
}

// The unresolved fallback must agree with what a rebuild would assign, or a
// consumer running before the first rebuild keys state under one identity and
// finds it under another afterwards.
func TestMotifAliases_UnresolvedClusterKeyMatchesTheRebuiltOne(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})

	before, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)

	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	after, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.Equal(t, before, after,
		"the pre-rebuild fallback and the stored key must be the same identity")
}

// TestMotifAliases_MN3_RebuildsIdenticallyAtEveryStage is the cross-session
// form of the MN3 guarantee: the derived layer is a pure function of (live
// facts, recorded judge decisions) at EVERY point in a corpus's history, not
// merely at the one moment a single-shot test happens to check.
//
// It lives here rather than with the other Phase-2 dynamics tests because
// dropping the derived layer needs the store's own handle, and a rebuild test
// that cannot first destroy what it is rebuilding proves only idempotence.
func TestMotifAliases_MN3_RebuildsIdenticallyAtEveryStage(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()

	stages := []struct {
		name string
		do   func()
	}{
		{"first fact", func() {
			writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
		}},
		{"an aliased spelling", func() {
			writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallbacks"})
		}},
		{"a judge merge", func() {
			require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
				"silent-fallback", "quiet-degradation", "one mechanism, two names"))
		}},
		{"the merged vocabulary arrives", func() {
			writeMotifFact(t, svc, branch, "kb/c.md", []string{"quiet-degradation"})
		}},
		{"an unrelated mechanism", func() {
			writeMotifFact(t, svc, branch, "kb/d.md", []string{"config-drift"})
		}},
		{"vocabulary retires", func() {
			writeMotifFact(t, svc, branch, "kb/c.md", []string{"config-drift"})
		}},
	}

	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md"}
	for _, stage := range stages {
		stage.do()
		require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

		before, err := svc.Motifs().AliasRows(ctx, branch)
		require.NoError(t, err)
		require.NotEmptyf(t, before, "%s: the stage must produce alias rows", stage.name)

		blobs := map[string]string{}
		for _, p := range paths {
			if rec, rerr := svc.FactQuery().GetByPath(ctx, branch, p); rerr == nil && rec != nil {
				blobs[p] = rec.BlobHash
			}
		}

		// Destroy the derived layer entirely. What remains is the facts and the
		// recorded decisions — the two inputs MN3 says it is a function of.
		_, err = svc.si.rh.db.ExecContext(ctx, `DELETE FROM motif_aliases`)
		require.NoError(t, err)
		empty, err := svc.Motifs().AliasRows(ctx, branch)
		require.NoError(t, err)
		require.Emptyf(t, empty, "%s: the drop must actually empty the table", stage.name)

		require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
		after, err := svc.Motifs().AliasRows(ctx, branch)
		require.NoError(t, err)
		require.Equalf(t, before, after,
			"%s: the derived layer must rebuild identically from facts and recorded "+
				"decisions alone", stage.name)

		for p, want := range blobs {
			rec, rerr := svc.FactQuery().GetByPath(ctx, branch, p)
			require.NoError(t, rerr)
			require.Equalf(t, want, rec.BlobHash,
				"%s: MN3 — rebuilding derived state must not rewrite %s", stage.name, p)
		}
	}
}
