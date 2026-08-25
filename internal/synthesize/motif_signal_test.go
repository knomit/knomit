package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// ── §7: the shortlist eligibility widener ─────────────────────────────────

// corpusSizeForBudget is the corpus size these tests report to the shortlist.
//
// It is passed as `n`, which selectRestatementCandidates uses for ONE thing:
// scaling the judge-slot budget (5 slots per thousand facts). Below 200 facts
// that budget is ZERO and the function returns before the widener is reached —
// which is exactly what happened to the first version of these tests. All three
// ran against a dozen facts, spent no slots, exercised nothing, and passed with
// the widener sabotaged.
//
// Reporting a realistic size lets a small fixture exercise the mechanism. The
// alternative — writing 200 facts per test — would test the budget arithmetic
// this file is not about, slowly.
const corpusSizeForBudget = 2000

// A shared canonical motif buys a pair a look FURTHER DOWN this repo's own
// ranking — it does not move the pair up that ranking, and it does not buy a
// bigger batch.
// The reserved slot (designer ruling Q10). A shared canonical motif is evidence
// ORTHOGONAL to title similarity, so the pairs it identifies are BELOW the
// title-ranked band by definition. Before the reservation the widener fired
// only when the ordinary band underfilled — i.e. never, in exactly the case it
// exists for. This is the test that could not be written before it.
func TestShortlistWidener_ReservedSlotFiresOnAFullBand(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	// Crowd facts crowd the TOP of the ranking with distinct bodies, so they
	// are candidates rather than near-duplicates and the ordinary band fills.
	// The motif pair sits further apart, so it ranks below them.
	env.emb.vectorFor = titleAxisVector("body", func(text string) float64 {
		switch {
		case strings.Contains(text, "Distant one"):
			return 0.90
		case strings.Contains(text, "Distant two"):
			return 0.95
		case strings.Contains(text, "Distant three"):
			return 0.93
		default:
			return 0.02 * unitHash(text)
		}
	})
	// Six crowd facts, not twelve: the crowd generates C(n,2) pairs, and too
	// many of them outrank the motif pair and push it past the WIDENED band
	// too — where the widener cannot see it either. The pair must land BETWEEN
	// the bands, which means bounding how many pairs sit above it.
	for i := range 6 {
		env.writeFact(fmt.Sprintf("kb/crowd%02d.md", i),
			fmt.Sprintf("Crowd member %d", i),
			fmt.Sprintf("A distinct body about subject number %d and its particulars.", i))
	}
	env.writeFactWithMotifs("kb/far1.md", "Distant one", "One body entirely of its own.",
		[]string{"silent-fallback"})
	env.writeFactWithMotifs("kb/far2.md", "Distant two", "Another body entirely of its own.",
		[]string{"silent-fallback"})
	// A THIRD sharer, so more than one widened pair is available. With only one
	// in supply, an off-by-one that let the reservation ENLARGE the budget
	// could not be observed — the loop would run out of widened pairs before
	// exceeding the cap, and the sabotage would pass.
	env.writeFactWithMotifs("kb/far3.md", "Distant three", "A third body of its own.",
		[]string{"silent-fallback"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold())
	require.NoError(t, err)

	// Budget >= 2, so a slot can be reserved without consuming the whole
	// budget. n is used for nothing but this.
	const corpus = 600
	require.GreaterOrEqual(t, shortlistBudget(corpus), 2,
		"precondition: the reservation is skipped at budget 1, by design")

	pairs, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, corpus)
	require.NoError(t, err)
	t.Logf("emitted=%d widened=%d slotUsed=%v standing=%d",
		h.Emitted, h.MotifWidened, h.MotifSlotUsed, h.StandingPairs)

	require.Positive(t, h.MotifWidened,
		"a pair below the title-ranked band sharing a canonical motif must take the "+
			"reserved slot — this is the whole signal, and before the reservation it "+
			"could not happen on a corpus whose ordinary band fills")
	require.Equal(t, shortlistBudget(corpus), len(pairs),
		"the reservation REALLOCATES within the budget; with supply on both sides "+
			"the emitted count must be exactly the budget, never more")
}

// At budget 1 the reservation is SKIPPED: the reserved slot would be the whole
// budget, and a corpus that can afford one judgment should spend it on its
// best-evidenced candidate rather than on orthogonal evidence about a
// lower-ranked one.
func TestShortlistWidener_NoReservationAtBudgetOne(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.emb.vectorFor = titleAxisVector("body", func(text string) float64 {
		return 0.02 * unitHash(text)
	})
	for i := range 12 {
		env.writeFactWithMotifs(fmt.Sprintf("kb/f%02d.md", i),
			fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{"silent-fallback"})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold())
	require.NoError(t, err)

	const corpus = 200
	require.Equal(t, 1, shortlistBudget(corpus), "precondition: this corpus funds one slot")

	pairs, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, corpus)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	require.Zero(t, h.MotifWidened,
		"the single funded slot must go to the top-ranked candidate, not to a "+
			"lower-ranked one carrying orthogonal evidence")
}

func TestShortlistWidener_IsInertWhenTheOrdinaryBandFillsTheBudget(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.emb.vectorFor = titleAxisVector("body", func(text string) float64 {
		return 0.02 * unitHash(text)
	})
	for i := range 30 {
		env.writeFactWithMotifs(fmt.Sprintf("kb/f%02d.md", i),
			fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{"silent-fallback"})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold())
	require.NoError(t, err)

	// Every fact shares a motif, so every pair is widenable — yet nothing is
	// widened, because the ordinary band fills the budget on its own.
	_, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, 200)
	require.NoError(t, err)
	require.Zero(t, h.MotifWidened,
		"with a full unexcluded ordinary band the widener must contribute nothing — "+
			"a merely-eligible pair still has to win a slot on rank, and it ranks last")
}

// The widener must NOT change the judge-slot budget. A motif-rich corpus gets
// better candidates for the same spend, never more of them.
func TestShortlistWidener_DoesNotEnlargeTheBudget(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Every fact shares one motif, so the widener applies to every pair it sees.
	for i := range 14 {
		env.writeFactWithMotifs("kb/f"+string(rune('a'+i))+".md", "Fact", "body",
			[]string{"silent-fallback"})
	}
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold())
	require.NoError(t, err)

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, corpusSizeForBudget)
	require.NoError(t, err)
	require.LessOrEqual(t, len(pairs), shortlistBudget(corpusSizeForBudget),
		"the widener changes ELIGIBILITY, never spend — a corpus where every pair "+
			"shares a motif must not be able to buy a bigger batch")
}

// Inert on a motif-free corpus: the ordinary band is exactly what it was.
func TestShortlistWidener_IsInertWithoutMotifs(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 12)
	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold())
	require.NoError(t, err)

	_, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, corpusSizeForBudget)
	require.NoError(t, err)
	require.Zero(t, h.MotifWidened,
		"a corpus with no motifs must widen nothing — the signal is an addition, "+
			"and an addition that fires on its own absence is a change")
}

// EXACT tier only. The loose tiers are for a reader who judges what comes back;
// this decides whether to spend a judge slot.
func TestShortlistWidener_UsesTheExactTierOnly(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Two facts whose motifs share a TOKEN but are different mechanisms.
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body", []string{"silent-retry"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	shared, err := pairSharesCanonicalMotif(ctx, env.deps(), env.branch,
		storeRestatementPair("kb/a.md", "kb/b.md"))
	require.NoError(t, err)
	require.False(t, shared,
		"a shared TOKEN is not a shared mechanism — token tiers must never reach "+
			"something that spends a judge slot")
}

// Aliased spellings DO share: that is what resolving the vocabulary bought.
func TestShortlistWidener_AliasedSpellingsShare(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body", []string{"silent-fallbacks"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	shared, err := pairSharesCanonicalMotif(ctx, env.deps(), env.branch,
		storeRestatementPair("kb/a.md", "kb/b.md"))
	require.NoError(t, err)
	require.True(t, shared,
		"two spellings of one mechanism are a shared motif — resolving the "+
			"vocabulary is what makes this true")
}

// ── §6: the distill enrichment line ───────────────────────────────────────

// SHARED, not merely present: a motif on one member says something about that
// member; a motif on several says something about the group, which is what the
// synthesized claim is about.
func TestDistillEnrichment_ListsOnlySharedMotifs(t *testing.T) {
	got := sharedClusterMotifs([]factForLLM{
		{File: "a", Motifs: []string{"silent-fallback", "only-mine-here"}},
		{File: "b", Motifs: []string{"silent-fallback"}},
		{File: "c", Motifs: []string{"config-drift"}},
	})
	require.Equal(t, "silent-fallback", got)
	require.NotContains(t, got, "only-mine-here", "one carrier is not a shared motif")
	require.NotContains(t, got, "config-drift")
}

// One fact is one carrier however often it repeats itself, or a single fact
// listing a motif twice would look like a shared one.
func TestDistillEnrichment_OneFactIsOneCarrier(t *testing.T) {
	require.Empty(t, sharedClusterMotifs([]factForLLM{
		{File: "a", Motifs: []string{"silent-fallback", "silent-fallback"}},
	}))
}

// A cluster sharing nothing gets no line at all — not an empty one.
func TestDistillEnrichment_NoSharedMotifsMeansNoLine(t *testing.T) {
	require.Empty(t, sharedClusterMotifs([]factForLLM{
		{File: "a", Motifs: []string{"silent-fallback"}},
		{File: "b", Motifs: []string{"config-drift"}},
	}))

	content, err := RenderDistillWorkItem([]factForLLM{{File: "a"}}, "kb", "")
	require.NoError(t, err)
	require.NotContains(t, content.Prompt, "Motifs already shared",
		"a motif-free cluster must see no motif line — free context that says "+
			"nothing is not free")
}

func TestDistillEnrichment_SharedMotifsReachThePrompt(t *testing.T) {
	content, err := RenderDistillWorkItem([]factForLLM{
		{File: "a", Motifs: []string{"silent-fallback"}},
		{File: "b", Motifs: []string{"silent-fallback"}},
	}, "kb", "")
	require.NoError(t, err)
	require.Contains(t, content.Prompt, "Motifs already shared by several of these facts")
	require.Contains(t, content.Prompt, "silent-fallback")
}

// storeRestatementPair builds the minimal pair the widener inspects — it reads
// only the two paths.
func storeRestatementPair(a, b string) store.RestatementPair {
	return store.RestatementPair{APath: a, BPath: b}
}

// titleAxisVector places a fact on the TITLE axis while leaving its BODY vector
// spread.
//
// The distinction is essential and cost several fixtures to learn. A standing
// restatement pair needs high title-cosine AND blended-cosine BELOW the dedup
// threshold — the mechanism exists to find facts that SAY the same thing while
// reading differently. A vectorFor that crowds every embedding call crowds the
// body vectors too, so every pair looks like a near-duplicate and the cache
// adds NOTHING. Diagnosed as PairsAdded: 0 after four fixtures that each looked
// plausible.
//
// EmbedShortStrings is called with the title alone; EmbedDocument with
// title + " " + body. Branching on the body marker is what keeps the two axes
// independent.
func titleAxisVector(bodyMarker string, place func(string) float64) func(string) []float32 {
	return func(text string) []float32 {
		if strings.Contains(text, bodyMarker) {
			return hashVector(text) // the BODY axis: spread, so pairs are not duplicates
		}
		return axisVector(place(text)) // the TITLE axis: placed by the test
	}
}
