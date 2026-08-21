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

// The vacuous-today property the ruling relied on: with no alias rows, every
// motif is its own singleton cluster and df is exactly what it was before
// aliases existed. Nothing regresses on a corpus that has never resolved.
func TestTokenDF_Motif_UnresolvedCorpusBehavesAsBefore(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})

	// No RebuildAliases call.
	df, err := svc.Search().TokenDF(ctx, branch, "silent-fallback", "motif")
	require.NoError(t, err)
	require.Equal(t, 1, df, "unresolved: each spelling is its own cluster")
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
