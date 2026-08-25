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
	// TWO SPELLINGS OF ONE MECHANISM, plus an unrelated third.
	//
	// The earlier fixture used "silent-fallback" twice and "config-drift" once:
	// three motifs, no two of which could possibly group together. Clusters and
	// VocabularyHealth were then compared on a corpus where the thing they
	// disagree about — COLLAPSING two spellings — could not arise, so the
	// comparison passed while the two APIs genuinely differed (Clusters split
	// them and claimed one key for both rows; health counted one). A fixture
	// where the values a test must distinguish coincide tests nothing about the
	// distinction.
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/c.md", []string{"config-drift"})
	// Deliberately NO RebuildAliases: this is the pre-first-session state, and
	// also the state of any corpus with a fact written since the last rebuild.

	// The precondition the fixture rests on, asserted rather than assumed: the
	// two spellings DIFFER and share a grouping key. A fixture that can
	// degenerate silently, will.
	require.NotEqual(t, "silent-fallback", "silent-fallbacks")
	require.Equal(t, groupingKey("silent-fallback"), groupingKey("silent-fallbacks"),
		"the fixture only exercises collapsing if the two spellings group together")
	require.NotEqual(t, groupingKey("silent-fallback"), groupingKey("config-drift"),
		"...and only exercises separation if the third does not")

	table, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.Empty(t, table, "precondition: the alias table must be unbuilt")

	clusters, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, clusters, 2,
		"an unresolved corpus still HAS a vocabulary — one mechanism spelled two ways "+
			"plus one other — and reporting none is a claim about the corpus that is false")

	// Every cluster key is distinct. Two rows sharing a key is the specific
	// shape of the bug: it means the grouping happened after the key did.
	keys := map[string]bool{}
	for _, c := range clusters {
		require.Falsef(t, keys[c.ClusterKey],
			"two clusters share the key %q — they are one cluster reported twice",
			c.ClusterKey)
		keys[c.ClusterKey] = true
	}

	// The collapsed cluster carries BOTH spellings and counts BOTH carriers.
	var collapsed MotifCluster
	for _, c := range clusters {
		if len(c.Members) > 1 {
			collapsed = c
		}
	}
	require.Equal(t, []string{"silent-fallback", "silent-fallbacks"}, collapsed.Members,
		"two spellings of one mechanism are one cluster, resolved or not — this is the "+
			"answer the next rebuild will give, and a reader should not see a different "+
			"one merely because no session has run yet")
	require.Equal(t, 2, collapsed.DF, "and its df spans both carriers")

	// Health sees the same vocabulary, because it computes the same key.
	vh, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, len(clusters), vh.Clusters,
		"Clusters and VocabularyHealth must agree on how many mechanisms this corpus has")
	require.Equal(t, 1, vh.Recurring, "the collapsed cluster has two carriers even unresolved")

	// EVERY reader agrees, including the point readers.
	//
	// This is the assertion the earlier version of this test reached for and
	// could not make honestly: its fixture spelled one motif twice, so nothing
	// could ever collapse and Clusters and TokenDF could not be caught
	// disagreeing. With spellings that DO group, they were caught — TokenDF
	// matched on canonical id and reported 1 where the cluster readers reported
	// 2 — and the designer ruled (2026-08-23) that the mechanical grouping key
	// is the floor in every reader at every moment.
	for _, c := range clusters {
		df, err := svc.Search().TokenDF(ctx, branch, c.CanonicalID, "motif")
		require.NoError(t, err)
		require.Equalf(t, df, c.DF,
			"Clusters and TokenDF must not disagree about %q on an unresolved corpus",
			c.CanonicalID)
	}

	// AND THE REBUILD CHANGES NOTHING. That is the ruling's real content: the
	// grouping is a pure function of the spellings, so the cache job records the
	// answer rather than producing it. A test that only checked the two readers
	// agree BEFORE the rebuild would pass for a pair that agreed on a wrong
	// answer and then both moved.
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	resolved, err := svc.Motifs().Clusters(ctx, branch)
	require.NoError(t, err)
	require.Len(t, resolved, 2, "the rebuild reproduces the same two clusters")
	for _, c := range resolved {
		var before MotifCluster
		for _, b := range clusters {
			if b.ClusterKey == c.ClusterKey {
				before = b
			}
		}
		require.Equalf(t, before.DF, c.DF,
			"cluster %q read df %d before the rebuild and %d after; the rebuild must "+
				"record the grouping, never change it", c.ClusterKey, before.DF, c.DF)
		require.Equalf(t, before.Members, c.Members,
			"and its membership must not move either", c.ClusterKey)
		rdf, err := svc.Search().TokenDF(ctx, branch, c.CanonicalID, "motif")
		require.NoError(t, err)
		require.Equalf(t, rdf, c.DF,
			"once resolved, Clusters and TokenDF must not disagree about %q", c.CanonicalID)
	}

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
