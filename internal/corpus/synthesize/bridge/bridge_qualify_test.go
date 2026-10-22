package bridge

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/graph"
	"knomit/internal/corpus/synthesize/step"
)

// TestBridgeKind_OnlyMechanismQualifies is the ruling in one assertion: a shared
// MECHANISM qualifies a bridge; a shared NAME does not.
func TestBridgeKind_OnlyMechanismQualifies(t *testing.T) {
	require.True(t, BridgeMotif.Qualifies(), "a shared mechanism qualifies")
	require.False(t, BridgeEntity.Qualifies(), "a shared entity name never qualifies")
	require.False(t, BridgeDomain.Qualifies(), "a shared domain tag never qualifies")
	require.False(t, BridgeBoth.Qualifies(), "neither does 'both' — it is the two non-qualifying axes")
	require.False(t, BridgeKind("").Qualifies(), "an unset kind qualifies nothing")
}

// TestBridgeKind_NoConfigValueCanRequalifyTheEntityAxis pins the OTHER half of
// why the entity axis is now silent, which is not in Qualifies() at all: the
// per-repo `discovery.bridge` knob cannot name the motif axis. BridgeMotif is
// deliberately absent from BridgeKindFromString, because motif bridging is
// bound to the effort dial rather than to config.
//
// Without this, a reader could conclude the silence is a misconfiguration a
// repo can fix by setting discovery.bridge = "motif". It cannot.
func TestBridgeKind_NoConfigValueCanRequalifyTheEntityAxis(t *testing.T) {
	for _, in := range []string{"", "domain", "entity", "both", "motif", "nonsense"} {
		require.Falsef(t, BridgeKindFromString(in).Qualifies(),
			"discovery.bridge = %q must not be able to make the token axis qualify", in)
	}
}

// ── the review surface ──

// TestReviewQualifyGate_HealthLineNamesTheDesignedState guards the CONTENT of
// the line rather than only its presence. The line exists to stop a reader
// diagnosing a stall, so it has to say both halves: what still qualifies, and
// that zero is designed.
func TestReviewQualifyGate_HealthLineNamesTheDesignedState(t *testing.T) {
	line := EntityAxisRankOnlyLine()
	require.Contains(t, line, "mechanism", "it must name what DOES qualify")
	require.Contains(t, strings.ToLower(line), "not a stall",
		"it must rule out the reading it exists to prevent")
	require.Contains(t, line, "#125", "it must be traceable to the ruling")
}

// ── the tiebreaker ──

// TestMotifRankLess_EntityOverlapBreaksTiesButNeverOutranks is a two-arm test
// and the SECOND arm is the load-bearing one.
//
// Arm 1 shows entity overlap doing its job among equals. Arm 2 shows it failing
// to do anything when Q differs — which is the whole difference between a
// tiebreaker and a weight, and the property that makes "entities never qualify"
// true of the rank order too. Under a WEnt weight in Q, arm 2 flips.
func TestMotifRankLess_EntityOverlapBreaksTiesButNeverOutranks(t *testing.T) {
	mk := func(token string, q, ent float64) enumeratedMotif {
		return enumeratedMotif{
			BridgeSeedSet: BridgeSeedSet{Token: token, Kind: BridgeMotif, Q: q},
			entOverlap:    ent,
		}
	}

	t.Run("equal Q: more entity overlap ranks first", func(t *testing.T) {
		// "a-token" sorts before "b-token", so Token-asc alone would put it
		// first — the tiebreaker has to actively reverse that to be visible.
		in := []enumeratedMotif{mk("a-token", 0.5, 0.1), mk("b-token", 0.5, 0.9)}
		sortMotifsForTest(in)
		require.Equal(t, "b-token", in[0].Token,
			"among equal-Q groups the one whose members share more entities is seen first")
	})

	t.Run("higher Q wins even with zero entity overlap", func(t *testing.T) {
		in := []enumeratedMotif{mk("a-token", 0.4, 1.0), mk("b-token", 0.5, 0.0)}
		sortMotifsForTest(in)
		require.Equal(t, "b-token", in[0].Token,
			"entity overlap must NEVER lift a weaker mechanism above a stronger one — "+
				"if this flips, the tiebreaker has become a weight and entities qualify again")
	})

	t.Run("equal Q and equal overlap falls back to Token asc", func(t *testing.T) {
		in := []enumeratedMotif{mk("b-token", 0.5, 0.5), mk("a-token", 0.5, 0.5)}
		sortMotifsForTest(in)
		require.Equal(t, "a-token", in[0].Token,
			"the determinism the rank order had before must survive the new term")
	})
}

// TestMotifRankLess_IsSharedByBothRankPaths pins that the served order and the
// measured order use ONE comparator. They drifted before (review finding M-4
// was the same class: a measurement surface reporting a disposition production
// never applied), and two sort literals is exactly how that recurs.
func TestMotifRankLess_IsSharedByBothRankPaths(t *testing.T) {
	mk := func(token string, q, ent float64) enumeratedMotif {
		return enumeratedMotif{
			BridgeSeedSet: BridgeSeedSet{Token: token, Kind: BridgeMotif, Q: q,
				Members: []step.FactForLLM{{File: token + "-x.md"}, {File: token + "-y.md"}}},
			entOverlap: ent,
		}
	}
	in := []enumeratedMotif{mk("a-token", 0.5, 0.1), mk("b-token", 0.5, 0.9)}

	viaRankAndCap, _ := rankAndCap(append([]enumeratedMotif{}, in...), 8)

	rows := make([]scoredMotifRow, 0, len(in))
	rowOf := map[string]int{}
	for i, c := range in {
		rows = append(rows, scoredMotifRow{cand: c, lane: LaneNear, q: c.Q, kept: true})
		rowOf[memberKey(c)] = i
	}
	viaRankAndCapRows, _ := rankAndCapRows(append([]enumeratedMotif{}, in...), 8, rows, rowOf)

	require.Len(t, viaRankAndCap, 2)
	require.Len(t, viaRankAndCapRows, 2)
	require.Equal(t, "b-token", viaRankAndCap[0].Token)
	require.Equal(t, viaRankAndCapRows[0].Token, viaRankAndCap[0].Token,
		"both rank paths must order identically — one comparator, no second literal")
}

// sortMotifsForTest applies the production comparator, so a test can never
// assert against a re-implementation of the order it is checking.
func sortMotifsForTest(in []enumeratedMotif) {
	sort.SliceStable(in, motifRankLess(in))
}

// TestMotifRankLess_EntityOverlapIsPopulatedByTheBuilder closes the gap the
// rank tests above cannot see.
//
// Those tests set entOverlap by hand, so they assert the COMPARATOR. Nothing in
// them asserts that production ever fills the field — and a sabotage deleting
// the single assignment in buildMotifBridgesWithRows passed every one of them,
// leaving the tiebreaker permanently inert at zero. Same shape as the lineage
// half's projection gap: a test that hands the value in is not a test of the
// wiring that produces it.
//
// This drives the real builder. Two candidates that tie on Q, one whose members
// share a high-df entity (so the subject-disjointness gate lets them stand —
// an umbrella label is ignored there, which is precisely the s183 shape where
// the motif signal was redundant with entity overlap) and one whose members
// share none.
func TestMotifRankLess_EntityOverlapIsPopulatedByTheBuilder(t *testing.T) {
	// "gartner" is carried by 100 of 200 live facts: far above the umbrella
	// cut, so the disjointness gate ignores it and both members survive.
	labels := labelsWith(200, map[string]int{"gartner": 100})

	seeds := []step.FactForLLM{
		// Group "a-shared": both members carry the umbrella entity.
		{File: "kb/alpha/1.md", Motifs: []string{"a-shared-shape"}, Entities: []string{"Gartner"}},
		{File: "kb/beta/2.md", Motifs: []string{"a-shared-shape"}, Entities: []string{"Gartner"}},
		// Group "b-disjoint": entity lists share nothing.
		{File: "kb/gamma/3.md", Motifs: []string{"b-disjoint-shape"}, Entities: []string{"Hapax1"}},
		{File: "kb/delta/4.md", Motifs: []string{"b-disjoint-shape"}, Entities: []string{"Hapax2"}},
		// A third recurring motif, needed only to clear the per-corpus
		// activation floor (3 recurring motifs). Both carriers sit in ONE
		// community, so the separation gate drops the group and it never
		// reaches the ranking this test is about — it pays the entry fee
		// without joining the race.
		{File: "kb/eps/5.md", Motifs: []string{"c-filler-shape"}, Entities: []string{"Hapax3"}},
		{File: "kb/zeta/6.md", Motifs: []string{"c-filler-shape"}, Entities: []string{"Hapax4"}},
	}
	clusters := graph.ClusterResult{Clusters: map[int][]string{
		0: {"kb/alpha/1.md"}, 1: {"kb/beta/2.md"},
		2: {"kb/gamma/3.md"}, 3: {"kb/delta/4.md"},
		4: {"kb/eps/5.md", "kb/zeta/6.md"},
	}}

	near, far, health, err := BuildMotifBridges(context.Background(), motifIdx(t, seeds, nil),
		"main", seeds, clusters, step.EffortHigh, motifQualityConfig(),
		identityResolver, labels, nil)
	require.NoError(t, err)

	served := append(append([]BridgeSeedSet{}, near...), far...)
	require.Lenf(t, served, 2,
		"fixture check: BOTH groups must survive every gate, or the ordering below "+
			"is not an ordering (candidates=%d)", health.Candidates)

	// Both groups score identically — same df, same (empty) adjacency, same
	// derivation gap — so Q cannot be what separates them.
	require.Equal(t, served[0].Q, served[1].Q,
		"fixture check: the two groups must TIE on Q, or this test would pass "+
			"on the Q term and never exercise the tiebreaker at all")

	// Token-ascending alone would put "a-shared-shape" first regardless, so the
	// assertion is made on the group that Token order already favours — which
	// means it can only be evidence together with the reverse check below.
	require.Equal(t, "a-shared-shape", served[0].Token,
		"the group whose members share an entity is served first among equals")

	// THE REVERSE CHECK: rename so Token order fights the tiebreaker. If
	// entOverlap is never populated, Token asc decides and "a-disjoint-shape"
	// wins; only a populated overlap can put the shared-entity group first.
	for i := range seeds {
		switch seeds[i].Motifs[0] {
		case "a-shared-shape":
			seeds[i].Motifs = []string{"z-shared-shape"}
		case "b-disjoint-shape":
			seeds[i].Motifs = []string{"a-disjoint-shape"}
		}
	}
	near, far, _, err = BuildMotifBridges(context.Background(), motifIdx(t, seeds, nil),
		"main", seeds, clusters, step.EffortHigh, motifQualityConfig(),
		identityResolver, labels, nil)
	require.NoError(t, err)
	served = append(append([]BridgeSeedSet{}, near...), far...)
	require.Len(t, served, 2)
	require.Equal(t, "z-shared-shape", served[0].Token,
		"with Token order pointing the other way, only a populated entity overlap can "+
			"still rank the shared-entity group first — if this fails, the builder is "+
			"not filling entOverlap and the tiebreaker is inert")
}
