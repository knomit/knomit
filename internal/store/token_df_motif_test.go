package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Q5 (designer ruling 2026-08-21): TokenDF's motif kind is CANONICAL-AWARE.
// It resolves the passed token through the alias table and counts distinct
// live facts across the cluster's member spellings.
//
// Phase 3 makes this load-bearing: BridgeSeedSet.Token is a canonical motif id
// and the df band is a GATE, so a df counting one spelling would bench a motif
// written three ways at df=1 three times — the exact failure alias resolution
// exists to prevent.

func TestTokenDF_Motif_CountsTheWholeCluster(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	id, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)

	df, err := svc.Search().TokenDF(ctx, branch, id, "motif")
	require.NoError(t, err)
	require.Equal(t, 2, df,
		"both spellings are one mechanism; df must be 2, not 1 per spelling")
}

// A fact carrying TWO spellings of the same cluster is ONE carrier. Summing
// per-spelling df in the caller would double-count it — the reason this
// resolution belongs inside TokenDF rather than in front of it.
func TestTokenDF_Motif_OneFactWithTwoSpellingsCountsOnce(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md",
		[]string{"silent-fallback", "silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	id, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)

	df, err := svc.Search().TokenDF(ctx, branch, id, "motif")
	require.NoError(t, err)
	require.Equal(t, 1, df, "one fact is one carrier however many spellings it uses")
}

// AN UNRESOLVED CORPUS READS THE WAY THE NEXT REBUILD WILL (designer ruling,
// 2026-08-23). The mechanical grouping key is the floor in every reader at
// every moment; the alias overlay sits on top of it.
//
// WHAT THIS TEST USED TO SAY, and why it changed. It asserted df 1 here —
// "unresolved: each spelling is its own cluster" — and that described the world
// before the alias rebuild ran in every Plan. While the rebuild was reachable
// only from the judge item's apply path (the C1 bootstrap deadlock), a corpus
// could sit unresolved indefinitely and a reader needed a standing answer for
// that state. The rebuild is now unconditional at the top of every
// medium-effort Plan, so "unresolved" is a window between a write and the next
// session rather than a state worth a second posture.
//
// It also contradicted this file's own Q5 header: stemming is a pure function
// of the spellings, so a reader that answers differently according to whether
// the cache job has run is reporting on the job, not on the corpus — and df
// counting one spelling is precisely the "written three ways at df=1 three
// times" failure the header says alias resolution exists to prevent.
func TestTokenDF_Motif_UnresolvedCorpusReadsAsTheRebuildWill(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})

	// No RebuildAliases call.
	df, err := svc.Search().TokenDF(ctx, branch, "silent-fallback", "motif")
	require.NoError(t, err)
	require.Equal(t, 2, df,
		"two spellings of one mechanism are one cluster whether or not a session has "+
			"written that down — the grouping is a pure function of the spellings, and "+
			"the rebuild caches it rather than creating it")

	// Either spelling names the cluster: the query token resolves through the
	// same steps as the stored ones.
	df, err = svc.Search().TokenDF(ctx, branch, "silent-fallbacks", "motif")
	require.NoError(t, err)
	require.Equal(t, 2, df, "and the other spelling gives the same answer")

	// THE RULING'S ACTUAL CLAIM: the answer does not move when the cache job
	// runs. Asserted by running it — a posture that merely happens to agree
	// today is not the property.
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	after, err := svc.Search().TokenDF(ctx, branch, "silent-fallback", "motif")
	require.NoError(t, err)
	require.Equal(t, 2, after,
		"RebuildAliases must not CHANGE this number, only record it")
}

// Distinct mechanisms must not pool their df.
func TestTokenDF_Motif_DistinctClustersDoNotPool(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	df, err := svc.Search().TokenDF(ctx, branch, "silent-fallback", "motif")
	require.NoError(t, err)
	require.Equal(t, 1, df)
}

// The other half of the ruling: the grouping key is a FLOOR, not a solvent.
// Token-disjoint spellings still merge only by a recorded judge decision, and
// that is true in the unresolved window too — where the floor is doing all the
// work and there is no overlay to hide behind.
func TestTokenDF_Motif_UnresolvedCorpusStillSeparatesDistinctMechanisms(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})

	// The precondition: these two share no stemmed tokens, so only a judge could
	// ever join them. A fixture where they happened to group would be testing
	// nothing about the distinction.
	require.NotEqual(t, groupingKey("silent-fallback"), groupingKey("quiet-degradation"),
		"precondition: token-disjoint spellings")

	// No RebuildAliases, no judge decision.
	df, err := svc.Search().TokenDF(ctx, branch, "silent-fallback", "motif")
	require.NoError(t, err)
	require.Equal(t, 1, df,
		"reading as the rebuild will must not become reading as the JUDGE might — "+
			"string similarity cannot join these, and only a recorded decision may")
}
