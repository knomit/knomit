package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
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

// CarrierTitles is MOST RECENT FIRST, and the order is load-bearing because
// the list is capped: whichever titles the cap admits are the entire evidence
// the judge gets, and explain's siblings inherit the same ordering.
//
// It documented most-recent-first and ordered alphabetically — a mismatch that
// reads as correct in both the doc and the code, and shows a cluster's oldest,
// least representative carriers whenever their titles happen to sort early.
func TestMotifClusters_CarrierTitlesAreMostRecentFirst(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	// Titles chosen so alphabetical ASC keeps the OLDEST carriers and recency
	// keeps the NEWEST — the two orderings must select DIFFERENT ends, or the
	// test cannot tell them apart.
	//
	// The first fixture here used descending-alphabetical titles, where both
	// orderings happened to drop the same one, and it passed with the ordering
	// sabotaged. Same defect as the seven earlier vacuous fixtures: two things
	// the code must distinguish producing the same answer.
	for i, title := range []string{"alpha", "bravo", "charlie", "delta"} {
		f := fact.NewFact(fmt.Sprintf("kb/c%d.md", i))
		f.Title = title
		f.Body = "body of " + title
		f.Type = fact.Observation
		f.Domain = []string{"alpha"}
		f.Entities = []string{"Widget"}
		f.Refs = []string{}
		f.Confidence = 0.8
		f.Sources = 1
		f.Motifs = []string{"silent-fallback"}
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, f.Path(), body, "seed "+title, "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	cs, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, cs, 1)

	// Cap at 2: the whole point is which end of the list the cap admits.
	titles, err := svc.Motifs().CarrierTitles(ctx, branch, cs[0].ClusterKey, 2)
	require.NoError(t, err)
	require.Len(t, titles, 2)
	require.NotContains(t, titles, "alpha",
		"the OLDEST carrier must not survive a cap of 2 — alphabetical ordering keeps "+
			"it and drops the most recent, which is the evidence the judge most needs")
	require.Contains(t, titles, "delta", "the newest carrier must be shown")
}

// ONE DEGRADATION POSTURE (review remediation). Every motif API must read the
// same vocabulary from a corpus whose alias table has not been built yet.
//
// The point readers (TokenDF, CanonicalID, ClusterKey, the query tiers) always
// degraded to "every motif is its own singleton cluster". The aggregate readers
// (Clusters, VocabularyHealth, CarrierTitles) returned NOTHING instead — so the
// same corpus reported a real vocabulary through one API and an empty one
// through another, and which you saw depended on which you asked.
//
// An empty alias table means "no review session has run yet": a transient
// bootstrap state, never a fact about the corpus.
func TestMotifClusters_UnresolvedCorpusReadsTheSameEverywhere(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/c.md", []string{"config-drift"})
	// Deliberately NO RebuildAliases: this is the pre-first-session state.

	table, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, table, "precondition: the alias table must be unbuilt")

	clusters, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, clusters, 2,
		"an unresolved corpus still HAS a vocabulary — two distinct motifs, each its "+
			"own singleton — and reporting none is a claim about the corpus that is false")

	// df agrees with what TokenDF reports for the same name.
	for _, c := range clusters {
		df, err := svc.Search().TokenDF(ctx, branch, c.CanonicalID, "motif")
		require.NoError(t, err)
		require.Equalf(t, df, c.DF,
			"Clusters and TokenDF must not disagree about %q on an unresolved corpus",
			c.CanonicalID)
	}

	// Health sees the same vocabulary.
	vh, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, len(clusters), vh.Clusters)
	require.Equal(t, 1, vh.Recurring, "silent-fallback has two carriers even unresolved")

	// And carriers resolve for a singleton key.
	var target MotifCluster
	for _, c := range clusters {
		if c.CanonicalID == "silent-fallback" {
			target = c
		}
	}
	titles, err := svc.Motifs().CarrierTitles(ctx, branch, target.ClusterKey, 10)
	require.NoError(t, err)
	require.Len(t, titles, 2, "both carriers of the unresolved motif must be reachable")
}
