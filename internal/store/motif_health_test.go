package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// writeMotifFactOrigin writes a fact with a chosen origin, for the
// authored-only assertions below.
func writeMotifFactOrigin(t *testing.T, svc *Service, branch, path string, motifs []string, origin fact.Origin) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "T " + path
	f.Body = "Body of " + path
	// origin x type is enforced in SerializeFact (invariant a63ff254): a
	// distilled fact must be a synthesis. The fixture obeys the same rule the
	// pipeline does rather than reaching around it.
	f.Type = fact.Observation
	if origin == fact.Distilled {
		f.Type = fact.Synthesis
	}
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 1
	f.Motifs = motifs
	f.Origin = origin
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

func TestVocabularyHealth_RecurrenceCountsClustersNotUses(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	// One recurring cluster (2 carriers) and two hapax.
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/c.md", []string{"config-drift"})
	writeMotifFact(t, svc, branch, "kb/d.md", []string{"cache-stampede"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	h, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 3, h.Clusters)
	require.Equal(t, 1, h.Recurring)
	require.InDelta(t, 1.0/3.0, h.RecurrenceRate(), 0.001)
}

// Recurrence and mint-to-link measure different things, and the fixture below
// is chosen so they point in OPPOSITE directions. If no such corpus existed,
// one of the two metrics would be redundant and should not be reported.
//
// The first version of this test used two corpora that happened to score worse
// on both — which proved nothing about independence and would have let a
// redundant metric ship looking validated.
//
//	deep: one motif with many carriers plus one hapax
//	      -> LOW recurrence (half the vocabulary never recurs)
//	      -> LOW mint-to-link (a great deal of reuse per name minted)
//	broad: many motifs, each with exactly two carriers
//	      -> HIGH recurrence (every name recurs)
//	      -> HIGHER mint-to-link (one reuse per name minted)
//
// A corpus can therefore look healthy on one and unhealthy on the other, which
// is exactly why §3.3 reports both.
func TestVocabularyHealth_RecurrenceAndMintRatioDiverge(t *testing.T) {
	ctx := context.Background()

	deepSvc, deepBranch := motifEnv(t)
	for i := range 8 {
		writeMotifFact(t, deepSvc, deepBranch, fmt.Sprintf("kb/deep%d.md", i),
			[]string{"silent-fallback"})
	}
	writeMotifFact(t, deepSvc, deepBranch, "kb/lonely.md", []string{"config-drift"})
	require.NoError(t, deepSvc.Motifs().RebuildAliases(ctx, deepBranch))
	deep, err := deepSvc.Motifs().VocabularyHealth(ctx, deepBranch)
	require.NoError(t, err)

	broadSvc, broadBranch := motifEnv(t)
	// Paths must NOT contain the motif's words: path tokens are subject tokens,
	// so "kb/silent-fallback-1.md" would strip the motif at write time and the
	// fixture would silently hold none at all.
	for i, m := range []string{"silent-fallback", "config-drift", "cache-stampede", "clock-skew"} {
		writeMotifFact(t, broadSvc, broadBranch, fmt.Sprintf("kb/x%d-1.md", i), []string{m})
		writeMotifFact(t, broadSvc, broadBranch, fmt.Sprintf("kb/x%d-2.md", i), []string{m})
	}
	require.NoError(t, broadSvc.Motifs().RebuildAliases(ctx, broadBranch))
	broad, err := broadSvc.Motifs().VocabularyHealth(ctx, broadBranch)
	require.NoError(t, err)

	require.Less(t, deep.RecurrenceRate(), broad.RecurrenceRate(),
		"deep has one recurring cluster in two; broad has four in four")
	require.Less(t, deep.MintToLinkRatio(), broad.MintToLinkRatio(),
		"...yet deep does MORE reuse per name minted. The two metrics point opposite "+
			"ways here, which is what makes reporting both worth the space")
}

// Authored-only is load-bearing: counting derived facts would report the
// engine's own carry-over as evidence that authors are converging.
func TestVocabularyHealth_ExcludesDerivedFacts(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	before, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 0, before.Recurring, "one authored carrier is not recurrence")

	// A DISTILLED fact carries the same motif. Recurrence must not move: the
	// pipeline was told to carry member motifs over, so this is the engine
	// agreeing with itself, not two authors converging.
	writeMotifFactOrigin(t, svc, branch, "kb/derived.md", []string{"silent-fallback"}, fact.Distilled)
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	after, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 0, after.Recurring,
		"a distilled fact carrying a motif forward must not count as recurrence — the "+
			"metric would then climb most on the corpora where the axis is doing least")
	require.Equal(t, before.Clusters, after.Clusters)
}

// The degenerate corpus must report zero rather than divide by it.
func TestVocabularyHealth_EmptyCorpusIsZeroNotNaN(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	h, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Zero(t, h.Clusters)
	require.Zero(t, h.RecurrenceRate())
	require.Zero(t, h.MintToLinkRatio())
}

// No reuse at all is the kill-switch state (§3.4) and must be REPORTED, not
// swallowed as a zero or an infinity — both of those read as "nothing to see".
func TestVocabularyHealth_NoReuseReportsTheMintCount(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	h, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Zero(t, h.Links)
	require.Equal(t, 2.0, h.MintToLinkRatio())
	require.Zero(t, h.RecurrenceRate(), "every write minting a name nothing reuses IS the dead axis")
}

// Aliased spellings are ONE cluster for health, or a corpus that spells a
// mechanism three ways would look like three failures to reuse.
func TestVocabularyHealth_AliasedSpellingsCountOnce(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"silent-fallbacks"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	h, err := svc.Motifs().VocabularyHealth(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 1, h.Clusters)
	require.Equal(t, 1, h.Recurring,
		"two spellings of one mechanism are recurrence, not two hapax — this is what "+
			"alias resolution exists to make visible")
	require.Equal(t, 1.0, h.RecurrenceRate())
}
