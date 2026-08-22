package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMotifClusters_OneRowPerClusterWithClusterDF(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	cs, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, cs, 2, "two spellings of one mechanism are ONE vocabulary entry")

	// Most frequent first.
	require.Equal(t, "silent-fallback", cs[0].CanonicalID)
	require.Equal(t, 2, cs[0].DF, "df counts carriers across the whole cluster")
	require.Equal(t, []string{"silent-fallback", "silent-fallbacks"}, cs[0].Members)
	require.Equal(t, "config-drift", cs[1].CanonicalID)
	require.Equal(t, 1, cs[1].DF)
}

// A fact using two spellings of one mechanism is ONE carrier, matching TokenDF.
// Two different counting rules for the same question would put the health
// metrics and the bridge gate on different numbers.
func TestMotifClusters_DFAgreesWithTokenDF(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md",
		[]string{"silent-fallback", "silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	cs, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, cs, 1)

	df, err := svc.Search().TokenDF(ctx, branch, cs[0].CanonicalID, "motif")
	require.NoError(t, err)
	require.Equal(t, df, cs[0].DF,
		"Clusters and TokenDF must not disagree about how many facts carry a mechanism")
	require.Equal(t, 2, cs[0].DF)
}

func TestMotifClusters_CarrierTitlesSpanEverySpelling(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	cs, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, cs, 1)

	titles, err := svc.Motifs().CarrierTitles(ctx, branch, cs[0].ClusterKey, 10)
	require.NoError(t, err)
	require.Len(t, titles, 2,
		"the judge must see carriers of BOTH spellings — a cluster shown through only "+
			"one of its members misrepresents what it covers")
	require.Contains(t, titles, "T kb/alpha/one.md")
	require.Contains(t, titles, "T kb/alpha/two.md")
}

func TestMotifClusters_CarrierTitlesRespectTheLimit(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	for _, p := range []string{"one", "two", "three", "four"} {
		writeMotifFact(t, svc, branch, "kb/alpha/"+p+".md", []string{"silent-fallback"})
	}
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	cs, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)

	titles, err := svc.Motifs().CarrierTitles(ctx, branch, cs[0].ClusterKey, 2)
	require.NoError(t, err)
	require.Len(t, titles, 2, "the prompt budget is a cap, not a suggestion")
}
