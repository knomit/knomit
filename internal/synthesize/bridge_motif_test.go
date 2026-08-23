package synthesize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// identityResolver is the alias table a corpus has before any judge pass: every
// spelling is its own cluster.
func identityResolver(m string) string { return m }

// constDF answers every df query with n — used where the df band is not what
// the test is about.
func constDF(n int) motifDFFn { return func(string) int { return n } }

// twoCommunities places each path in its own cluster, which is what makes a
// pair a BRIDGE rather than an in-cluster neighbour.
func twoCommunities(a, b string) ClusterResult {
	return ClusterResult{Clusters: map[int][]string{0: {a}, 1: {b}}}
}

func TestEnumerateMotifCandidates_GroupsOnCanonicalIDAcrossSpellings(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/gotchas/uitesting/1.md", Motifs: []string{"measure-becomes-target"},
			Entities: []string{"Cognition"}},
		{File: "kb/technology/rewardhacking/2.md", Motifs: []string{"metric-becomes-target"},
			Entities: []string{"RLHF"}},
	}
	// The alias judge merged the two spellings; enumeration keys on the result.
	resolve := func(string) string { return "measure-becomes-target" }

	got, h := enumerateMotifCandidates(seeds,
		twoCommunities("kb/gotchas/uitesting/1.md", "kb/technology/rewardhacking/2.md"),
		resolve, constDF(2), labelsWith(200, map[string]int{"cognition": 2, "rlhf": 3}), tierExact)

	require.Len(t, got, 1)
	require.Equal(t, "measure-becomes-target", got[0].Token, "Token is the CANONICAL id")
	require.Equal(t, BridgeMotif, got[0].Kind)
	require.Len(t, got[0].Members, 2)
	require.Equal(t, 1, h.Candidates)
}

func TestEnumerateMotifCandidates_DFBandExcludesBothEnds(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"hapax-shape", "generic-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"hapax-shape", "generic-shape"}, Entities: []string{"Beta"}},
	}
	// N=200 => ceiling max(12, 2%*200) = 12. One motif below the floor, one above.
	df := func(c string) int {
		if c == "hapax-shape" {
			return 1
		}
		return 40
	}

	got, h := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, df, labelsWith(200, nil), tierExact)

	require.Empty(t, got, "df=1 cannot bridge yet; df=40 has gone generic")
	require.Equal(t, 12, h.Ceiling)
	require.Equal(t, []string{"generic-shape"}, h.OverCeilingNames,
		"over-ceiling is FLAGGED for review splitting, not silently dropped (MN8)")
}

func TestEnumerateMotifCandidates_DFCeilingScalesWithTheCorpus(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")

	// df 30 is over the ceiling on a 200-fact corpus (12) and under it on a
	// 3000-fact one (60): the band is a property of the corpus, not a constant.
	small, hs := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(30),
		labelsWith(200, nil), tierExact)
	large, hl := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(30),
		labelsWith(3000, nil), tierExact)

	require.Equal(t, 12, hs.Ceiling)
	require.Equal(t, 60, hl.Ceiling)
	require.Empty(t, small)
	require.Len(t, large, 1)
}

func TestEnumerateMotifCandidates_RequiresTwoCommunities(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	// Both in one community: separation 1. A GATE, never a reward (1f536807/MN8).
	same := ClusterResult{Clusters: map[int][]string{0: {"kb/alpha/1.md", "kb/beta/2.md"}}}

	got, _ := enumerateMotifCandidates(seeds, same, identityResolver, constDF(2),
		labelsWith(200, nil), tierExact)
	require.Empty(t, got)
}

// MN7's fixture asserted at the enumeration boundary the consumer actually
// reads, not only at the gate helper.
func TestEnumerateMotifCandidates_DropsTheNearDuplicatePair(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/technology/ai/agents/1.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition", "Devin"}},
		{File: "kb/technology/ai/agents/2.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition", "Devin"}},
	}
	labels := labelsWith(200, map[string]int{
		"cognition": 2, "devin": 2, "agent": 30, "technology": 40, "ai": 35})

	got, _ := enumerateMotifCandidates(seeds,
		twoCommunities("kb/technology/ai/agents/1.md", "kb/technology/ai/agents/2.md"),
		identityResolver, constDF(2), labels, tierExact)

	require.Empty(t, got, "MN7: the axis must not re-find the 8ebd5d90 population")
}

// The complement: the same shape, subject-disjoint, DOES enumerate. Without
// this the test above would pass on a gate that rejected everything.
func TestEnumerateMotifCandidates_KeepsTheSubjectDisjointPair(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/gotchas/uitesting/1.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition"}, Domain: []string{"evaluation"}},
		{File: "kb/technology/rewardhacking/2.md", Motifs: []string{"shared-shape"},
			Entities: []string{"RLHF"}, Domain: []string{"evaluation"}},
	}
	labels := labelsWith(200, map[string]int{"evaluation": 30, "cognition": 2, "rlhf": 3})

	got, _ := enumerateMotifCandidates(seeds,
		twoCommunities("kb/gotchas/uitesting/1.md", "kb/technology/rewardhacking/2.md"),
		identityResolver, constDF(2), labels, tierExact)

	require.Len(t, got, 1, "one shared coarse tag must not block a genuine bridge")
}

// A group of three where ONE member shares a subject with another: the offender
// drops, the bridge survives. All-or-nothing rejection would let a single
// near-duplicate delete a genuine three-way group.
func TestEnumerateMotifCandidates_DropsOffendingMemberNotTheGroup(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
		// Same rare entity as the first member — a near-duplicate of it.
		{File: "kb/alpha/3.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
	}
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/alpha/1.md"}, 1: {"kb/beta/2.md"}, 2: {"kb/alpha/3.md"}}}
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	got, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(3), labels, tierExact)

	require.Len(t, got, 1)
	require.Len(t, got[0].Members, 2, "the third member is the first one's near-duplicate")
	require.Equal(t, "kb/alpha/1.md", got[0].Members[0].File, "path-sorted, so the survivor is deterministic")
	require.Equal(t, "kb/beta/2.md", got[0].Members[1].File)
}

func TestEnumerateMotifCandidates_Token2TierGroupsDistinctCanonicalIDs(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"stale-cache-capture"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"cache-capture-drift"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	exact, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
	require.Empty(t, exact, "distinct canonical ids do not match verbatim")

	loose, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierToken2)
	require.Len(t, loose, 1, "two shared stemmed tokens (cache, capture) match at token-2")
	require.Len(t, loose[0].Members, 2)
}

func TestEnumerateMotifCandidates_Token2NeedsTwoSharedTokens(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"stale-cache-capture"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"cache-warming-strategy"}, Entities: []string{"Beta"}},
	}
	got, _ := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, constDF(2), labelsWith(200, map[string]int{"alpha": 2, "beta": 2}), tierToken2)
	require.Empty(t, got, "one shared token (cache) is below the token-2 tier")
}

// §7 idempotency (cf455b8f): discovery never feeds on its own output.
func TestEnumerateMotifCandidates_ExcludesDiscoveredOrigin(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"},
			Origin: "discovered"},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	got, _ := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, constDF(2), labelsWith(200, map[string]int{"alpha": 2, "beta": 2}), tierExact)
	require.Empty(t, got)
}

func TestEnumerateMotifCandidates_IsDeterministic(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"bravo-shape", "alpha-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"bravo-shape", "alpha-shape"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	first, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
	for i := 0; i < 20; i++ {
		again, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
		require.Equal(t, first, again, "map iteration order must not reach the output")
	}
	require.Equal(t, "alpha-shape", first[0].Token, "token-sorted")
}
