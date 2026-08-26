package synthesize

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
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

// TestReviewQualifyGate_EntityBridgesAreNotEnqueued is the behavioural half,
// and it is built as a PAIR of assertions on ONE fixture so it cannot pass
// vacuously:
//
//  1. the builder, run over the same seeds, DOES produce an entity/domain
//     bridge — so the corpus provably would have served discover items before
//     this change, and a green result is not "the fixture never bridged";
//  2. the session nevertheless enqueues NO discover item, and says why.
//
// Assertion 1 is the load-bearing one. Without it this test would stay green if
// the seeds stopped bridging for any unrelated reason — the fixture-vacuity
// shape that has cost this campaign four fixtures.
func TestReviewQualifyGate_EntityBridgesAreNotEnqueued(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	// Four facts on one shared domain token, no motifs anywhere — the exact
	// shape #125 measured on core, and a corpus the motif axis cannot serve.
	paths := []string{"kb/alpha/one.md", "kb/beta/two.md", "kb/gamma/three.md", "kb/delta/four.md"}
	seeds := make([]fact.Fact, 0, len(paths))
	for i, p := range paths {
		f := fact.NewFact(p)
		f.Title, f.Body, f.Type = p, "body of "+p, fact.Observation
		f.Domain, f.Confidence, f.Sources = []string{"auth"}, 0.5, 1
		f.Entities = []string{[]string{"Alpha", "Beta", "Gamma", "Delta"}[i]}
		body, serr := fact.SerializeFact(f)
		require.NoError(t, serr)
		_, werr := svc.Facts().WriteFact(ctx, branch, f.Path(), body, "seed", "")
		require.NoError(t, werr)
		seeds = append(seeds, f)
	}

	// (1) FIXTURE CHECK — these seeds really do form an entity/domain bridge.
	llm := factsForLLM(seeds, testLocalRepoID)
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {paths[0], paths[1]}, 1: {paths[2], paths[3]},
	}}
	cands := enumerateBridgeCandidates(llm, clusters, BridgeBoth, ScopeFilter{})
	_, wouldBridge := containsToken(cands, "auth")
	require.True(t, wouldBridge,
		"fixture check: this corpus DOES produce a domain bridge — without this, "+
			"the absence asserted below would prove nothing")

	// (2) BEHAVIOUR — a real high-effort session serves none of it.
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	r := NewReviewerWithEffort(ri, nil, EffortHigh)

	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items, err := svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, items,
		"the session must have queued its grounded work — an empty queue would make "+
			"the discover-absence below meaningless")
	for _, it := range items {
		require.NotEqualf(t, "discover", it.StepType,
			"no discover item may be enqueued from a mechanism-free corpus (%s)", it.ClusterKey)
	}

	// (3) AND IT SAYS WHY. A zero that does not explain itself is the
	// saturated-vs-broken confusion this campaign already paid for once.
	require.Contains(t, strings.Join(res.Health, "\n"), "RANK-ONLY",
		"the session must state that the entity/domain axis cannot qualify, so a "+
			"later auditor does not read zero discover items as a stall")
}

// TestReviewQualifyGate_HealthLineNamesTheDesignedState guards the CONTENT of
// the line rather than only its presence. The line exists to stop a reader
// diagnosing a stall, so it has to say both halves: what still qualifies, and
// that zero is designed.
func TestReviewQualifyGate_HealthLineNamesTheDesignedState(t *testing.T) {
	line := entityAxisRankOnlyLine()
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
				Members: []factForLLM{{File: token + "-x.md"}, {File: token + "-y.md"}}},
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

// TestDiscoverPayloadShapeUnchanged is a cheap guard on the one thing the
// qualify gate must NOT have changed: a discover item's payload. The gate
// decides WHETHER an item is made, never what is in it.
func TestDiscoverPayloadShapeUnchanged(t *testing.T) {
	b, err := json.Marshal(DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge:    BridgeSeedSet{Token: "x", Kind: BridgeMotif},
	})
	require.NoError(t, err)
	require.Contains(t, string(b), `"kind":"motif"`)
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

	seeds := []factForLLM{
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
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/alpha/1.md"}, 1: {"kb/beta/2.md"},
		2: {"kb/gamma/3.md"}, 3: {"kb/delta/4.md"},
		4: {"kb/eps/5.md", "kb/zeta/6.md"},
	}}

	near, far, health, err := buildMotifBridges(context.Background(), &countingSearchIndex{},
		"main", seeds, clusters, EffortHigh, motifQualityConfig(),
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
	near, far, _, err = buildMotifBridges(context.Background(), &countingSearchIndex{},
		"main", seeds, clusters, EffortHigh, motifQualityConfig(),
		identityResolver, labels, nil)
	require.NoError(t, err)
	served = append(append([]BridgeSeedSet{}, near...), far...)
	require.Len(t, served, 2)
	require.Equal(t, "z-shared-shape", served[0].Token,
		"with Token order pointing the other way, only a populated entity overlap can "+
			"still rank the shared-entity group first — if this fails, the builder is "+
			"not filling entOverlap and the tiebreaker is inert")
}

// TestHypothesizeQualifyGate_BackwardBridgesAreNotEnqueued covers the SECOND
// surface the qualify gate darkens, and it is the one a reader is most likely
// to misdiagnose.
//
// The hypothesize tool's own discovery is entity/domain-ONLY: the motif axis is
// enumerated in review, over the ordinary seed pool where the motif-carrying
// facts live, and its far lane already routes backward from there. So unlike
// the review surface — which still serves motif bridges on a motif-rich corpus
// — this one now yields nothing on EVERY corpus at EVERY effort. Backward
// discovery is not lost; this path to it is.
//
// Built as a pair, same discipline as the review test: prove the pool would
// have bridged, then prove nothing was served, then prove the session says why.
func TestHypothesizeQualifyGate_BackwardBridgesAreNotEnqueued(t *testing.T) {
	ctx := context.Background()
	svc, ri := newHypothesizeTestRepo(t)

	// Four synthesis facts sharing one domain token across two communities —
	// the pool BuildBackwardBridges is built to bridge.
	paths := []string{"kb/alpha/s1.md", "kb/beta/s2.md", "kb/gamma/s3.md", "kb/delta/s4.md"}
	for _, p := range paths {
		writeTestFact(t, svc, p, p, fact.Synthesis, "auth")
	}

	// FIXTURE CHECK — the same pool really does enumerate a domain bridge.
	// Without it, "no discover items" would be indistinguishable from "these
	// four facts never formed a candidate".
	llm := make([]factForLLM, 0, len(paths))
	for _, p := range paths {
		llm = append(llm, factForLLM{File: p, Domain: []string{"auth"}, Origin: "authored"})
	}
	cands := enumerateBridgeCandidates(llm, ClusterResult{Clusters: map[int][]string{
		0: {paths[0], paths[1]}, 1: {paths[2], paths[3]},
	}}, BridgeBoth, ScopeFilter{})
	_, wouldBridge := containsToken(cands, "auth")
	require.True(t, wouldBridge,
		"fixture check: this synthesis pool DOES enumerate a domain bridge")

	p := NewHypothesizer(ri, nil, EffortHigh, ScopeFilter{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items, err := svc.Pipeline().PendingPipelineWorkItems(ctx, res.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, items,
		"the session must have queued its per-fact work — an empty queue would make "+
			"the discover-absence below meaningless")
	for _, it := range items {
		require.NotEqualf(t, "discover", it.StepType,
			"the hypothesize surface can no longer qualify a bridge (%s)", it.ClusterKey)
	}

	// Health is IN-MEMORY ONLY on the session and rides the FIRST result, so
	// it is read from res rather than from the stored row.
	require.Contains(t, strings.Join(res.Health, "\n"), "RANK-ONLY",
		"this surface is dark on EVERY corpus, so it must say so — a reader who "+
			"finds zero discover items here has no other way to tell design from stall")
}
