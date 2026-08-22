package synthesize

import (
	"context"
	"fmt"
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
// NOTE — there is deliberately NO end-to-end test here showing the widener
// admitting a pair, and its absence is a finding rather than a gap in effort.
//
// Selection walks the ranking in order and stops at the judge-slot budget. A
// widened pair sits BELOW the ordinary band by construction, so it can only be
// reached when the ordinary band fails to fill the budget. Constructing that
// state requires a corpus where the standing pair cache holds more pairs than
// the widened band is deep, the ordinary band's pairs are all excluded, and the
// motif-sharing pair ranks between the two bands. Four attempts at that fixture
// produced four differently-vacuous tests, and the difficulty is the point: if
// the condition is this hard to arrange deliberately, it is rare accidentally.
//
// The mechanism's decision logic IS tested — pairSharesCanonicalMotif, both
// directions, below — and the inert case is asserted. What is not asserted is
// the widener contributing on a realistic corpus, because the measurement says
// it usually cannot. Raised with the designer: making it fire more often means
// either a score bonus (rejected as a corpus-property constant in disguise,
// Q2) or reserving a slot for widened pairs, which is a new decision and not
// mine to take.

// WHEN THE WIDENER ACTUALLY FIRES// The inert case, measured: with a full unexcluded ordinary band the widener
// contributes nothing, and that follows directly from "eligibility widener,
// budget unchanged, no score bonus" — a merely-ELIGIBLE pair still has to win a
// slot on rank, and it ranks last by definition.
func TestShortlistWidener_IsInertWhenTheOrdinaryBandFillsTheBudget(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.emb.vectorFor = func(text string) []float32 { return axisVector(0.02 * unitHash(text)) }
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
