package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The §6 motif_match knob. Ordered by strictness, default strictest: loose
// tiers are for a reader who judges what comes back, never for automation.

func motifFilterEnv(t *testing.T) (*Service, string) {
	t.Helper()
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/two.md", []string{"silent-fallbacks"})
	writeMotifFact(t, svc, branch, "kb/three.md", []string{"silent-retry"})
	writeMotifFact(t, svc, branch, "kb/four.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	return svc, branch
}

func motifSearch(t *testing.T, svc *Service, branch string, motif string, tier MotifMatchTier) []string {
	t.Helper()
	// Explicit non-zero Limit: a bare SearchOptions carries Limit 0, which
	// becomes a literal LIMIT 0 and makes every assertion below vacuous
	// (gotchas/store/testing/searchoptions-zero-limit/71123f5f).
	res, err := svc.FactQuery().Search(context.Background(), branch, SearchOptions{
		Motifs:     []string{motif},
		MotifMatch: tier,
		Limit:      50,
	})
	require.NoError(t, err)
	var paths []string
	for _, r := range res {
		paths = append(paths, r.Path)
	}
	return paths
}

// Exact resolves through the alias table, so a query naming ANY member
// spelling finds facts carrying any other. That is what having resolved the
// vocabulary buys.
func TestMotifMatch_ExactSpansAliasedSpellings(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	got := motifSearch(t, svc, branch, "silent-fallbacks", MotifMatchExact)
	require.ElementsMatch(t, []string{"kb/one.md", "kb/two.md"}, got,
		"both spellings are one mechanism; querying either must find both")
}

// The DEFAULT is exact. A caller who names no tier gets the strictest one.
func TestMotifMatch_DefaultIsExact(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	explicit := motifSearch(t, svc, branch, "silent-fallback", MotifMatchExact)
	implicit := motifSearch(t, svc, branch, "silent-fallback", "")
	require.ElementsMatch(t, explicit, implicit,
		"an unspecified tier must behave as exact, never as something looser")
	require.NotContains(t, implicit, "kb/three.md",
		"silent-retry shares a token with silent-fallback and must NOT match by default")
}

// token-1 is the loosest mechanical tier and reaches strictly further.
func TestMotifMatch_TiersAreOrderedByStrictness(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	exact := motifSearch(t, svc, branch, "silent-fallback", MotifMatchExact)
	token1 := motifSearch(t, svc, branch, "silent-fallback", MotifMatchToken1)

	require.Subset(t, token1, exact, "every tier must include what the stricter one found")
	require.Contains(t, token1, "kb/three.md",
		"token-1 shares only 'silent' with silent-retry, and that is the tier's whole point")
	require.NotContains(t, token1, "kb/four.md", "config-drift shares no token")
}

// token-2 needs two shared tokens, so a single-token overlap does not qualify.
func TestMotifMatch_Token2NeedsTwoSharedTokens(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	got := motifSearch(t, svc, branch, "silent-fallback", MotifMatchToken2)
	require.NotContains(t, got, "kb/three.md",
		"silent-retry shares only one token and must not match at token-2")
	require.Contains(t, got, "kb/one.md")
}

// stem is cluster-key equality — the alias layer's own grouping key, asked as a
// different question.
func TestMotifMatch_StemMatchesTheClusterKey(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	got := motifSearch(t, svc, branch, "silent-fallbacks", MotifMatchStem)
	require.ElementsMatch(t, []string{"kb/one.md", "kb/two.md"}, got)
}

// soft is declared and gated but not calibrated. It must match NOTHING rather
// than quietly behaving like another tier — a caller who asked for soft and
// received exact's results has been answered under a false name.
func TestMotifMatch_SoftIsGatedNotSilentlyDowngraded(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	got := motifSearch(t, svc, branch, "silent-fallback", MotifMatchSoft)
	require.Empty(t, got,
		"an uncalibrated tier must return nothing a caller could mistake for an answer")
}

// The filter is INERT unless asked for. Every ordinary search must be
// byte-identical to what it was before motifs existed.
func TestMotifMatch_NoMotifFilterMeansNoMotifClause(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	res, err := svc.FactQuery().Search(context.Background(), branch, SearchOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, res, 4, "a search naming no motif must be unaffected by the axis")
}

// A motif nothing carries matches nothing — not everything.
func TestMotifMatch_UnknownMotifMatchesNothing(t *testing.T) {
	svc, branch := motifFilterEnv(t)
	got := motifSearch(t, svc, branch, "no-such-mechanism", MotifMatchExact)
	require.Empty(t, got)
}

// On a corpus whose aliases were never built, exact degrades to plain string
// equality rather than matching nothing.
func TestMotifMatch_UnresolvedCorpusStillMatchesExactly(t *testing.T) {
	svc, branch := motifEnv(t)
	writeMotifFact(t, svc, branch, "kb/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/two.md", []string{"config-drift"})
	// Deliberately NO RebuildAliases.

	got := motifSearch(t, svc, branch, "silent-fallback", MotifMatchExact)
	require.Equal(t, []string{"kb/one.md"}, got,
		"before the vocabulary is resolved every motif is its own cluster, and the "+
			"filter must still work rather than returning nothing")
}

// TIER ORDERING AFTER A JUDGE MERGE — the case the tier ordering was silently
// broken for, and which no test covered.
//
// A merge sets every member's cluster_key to min() of the union, so the LOSING
// key's own grouping key no longer equals what the table stores for it.
// Comparing the raw grouping key returned NOTHING for the losing spelling
// while exact returned the whole cluster — stem was not merely narrower than
// exact, it was empty, which inverts "loosest last" entirely.
//
// The rule, asserted for EVERY spelling rather than a representative one:
// each tier must include what the stricter tier found.
func TestMotifMatch_TierOrderingHoldsAfterAJudgeMerge(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation", "both name serving on after a failure"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	// Both spellings, because the defect was asymmetric: only the losing key
	// broke, so a test that checked one spelling had a 50% chance of passing.
	for _, term := range []string{"silent-fallback", "quiet-degradation"} {
		exact := motifSearch(t, svc, branch, term, MotifMatchExact)
		require.ElementsMatchf(t, []string{"kb/a.md", "kb/b.md"}, exact,
			"exact must span the merged cluster from %q", term)

		for _, looser := range []MotifMatchTier{MotifMatchStem, MotifMatchToken2, MotifMatchToken1} {
			got := motifSearch(t, svc, branch, term, looser)
			require.Subsetf(t, got, exact,
				"tier %q must include everything exact found for %q — the tiers are "+
					"ordered by strictness, and a looser one returning less is not a "+
					"narrower answer but a wrong one", looser, term)
		}
	}
}
