package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// testLocalRepoID is a well-formed 12-hex repo id. It is a REAL id shape, not
// "", because "" is the configuration under which the lineage exclusion does
// nothing at all (see TestBridgeLineage_EmptyRepoIDExcludesNothing) — a suite
// that passed "" everywhere would be asserting the gate's behaviour with the
// gate switched off.
const testLocalRepoID = "abcdef012345"

// storedRef renders a ref the way the corpus actually stores one: canonical
// kb://<own-id12>/<path>, NOT a bare path. Every lineage test that means to
// exercise production spelling uses this rather than writing the path raw.
func storedRef(path string) string { return fact.QualifyKBPath(testLocalRepoID, path) }

// lineageSeed is a two-community fixture member: it carries the shared token
// the bridge would form on, plus whatever refs the test wants it to cite.
func lineageSeed(path string, refs ...string) factForLLM {
	return factForLLM{
		File:        path,
		Title:       path,
		Body:        path + " body",
		Type:        "observation",
		Domain:      []string{"auth"},
		Origin:      "authored",
		LineageRefs: localFactRefPaths(refs, testLocalRepoID),
	}
}

// ── the control: the fixture must produce a bridge when lineage is absent ──

// TestBridgeLineage_ControlUnrelatedPairIsABridge is the falsifiability anchor
// for every exclusion test below. The same two facts, in the same two
// communities, on the same shared token, with NO citation between them, MUST
// bridge. Without this, an exclusion test cannot tell "the gate dropped it"
// from "the fixture never formed a candidate in the first place" — the
// unreachable-branch shape that has cost this campaign three fixtures.
func TestBridgeLineage_ControlUnrelatedPairIsABridge(t *testing.T) {
	parent := lineageSeed("kb/synthesis/parent.md")
	child := lineageSeed("kb/observations/child.md")

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child},
		twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md"),
		BridgeDomain, ScopeFilter{})

	set, found := containsToken(got, "auth")
	require.True(t, found, "control: an uncited cross-community pair on a shared domain MUST bridge")
	require.Len(t, set.Members, 2)
}

// TestBridgeLineage_ChildCitingParentIsNotABridge is #125's measured case: a
// synthesis inherits its members' domain tags, so the domain token reconnects
// it to the very facts it was distilled from. Reinforcing that group raises
// `sources` on evidence the fact was already built from.
func TestBridgeLineage_ChildCitingParentIsNotABridge(t *testing.T) {
	parent := lineageSeed("kb/synthesis/parent.md")
	child := lineageSeed("kb/observations/child.md", storedRef("kb/synthesis/parent.md"))

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child},
		twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md"),
		BridgeDomain, ScopeFilter{})

	_, found := containsToken(got, "auth")
	require.False(t, found,
		"a pair whose one member CITES the other is lineage, not a bridge: %+v", got)
}

// TestBridgeLineage_ParentCitingChildIsNotABridge pins the OTHER direction.
// It is not symmetry for its own sake: the two directions arise from different
// mechanisms — a distilled fact cites its sources (child→parent), and a merged
// fact cites the predecessors it absorbed (parent→child) — so a one-directional
// gate would silently pass a whole class.
func TestBridgeLineage_ParentCitingChildIsNotABridge(t *testing.T) {
	parent := lineageSeed("kb/synthesis/parent.md", storedRef("kb/observations/child.md"))
	child := lineageSeed("kb/observations/child.md")

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child},
		twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md"),
		BridgeDomain, ScopeFilter{})

	_, found := containsToken(got, "auth")
	require.False(t, found, "citation in EITHER direction is lineage: %+v", got)
}

// TestBridgeLineage_EmptyRepoIDExcludesNothing is the load-bearing test of this
// file, and it asserts a FAILURE MODE rather than a feature.
//
// Refs are STORED CANONICAL as kb://<own-id12>/<path>. Classified with an empty
// local repo id, every one of them reads as a FOREIGN fact ref, localFactRefPaths
// returns nothing, and the exclusion has an empty set to test membership
// against — so it excludes zero pairs while every other test in the suite still
// passes. That is a whole fix shipping green and doing nothing.
//
// The two arms below are the SAME fixture and differ only in the repo id, so
// what they measure is the id and nothing else.
func TestBridgeLineage_EmptyRepoIDExcludesNothing(t *testing.T) {
	raw := []string{storedRef("kb/synthesis/parent.md")}
	clusters := twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md")

	withID := []factForLLM{
		{File: "kb/synthesis/parent.md", Domain: []string{"auth"}, Origin: "authored"},
		{File: "kb/observations/child.md", Domain: []string{"auth"}, Origin: "authored",
			LineageRefs: localFactRefPaths(raw, testLocalRepoID)},
	}
	require.NotEmpty(t, withID[1].LineageRefs,
		"fixture check: with the real repo id the stored ref MUST reduce to a local path")
	_, found := containsToken(
		enumerateBridgeCandidates(withID, clusters, BridgeDomain, ScopeFilter{}), "auth")
	require.False(t, found, "with the repo id the lineage pair is excluded")

	withoutID := []factForLLM{
		{File: "kb/synthesis/parent.md", Domain: []string{"auth"}, Origin: "authored"},
		{File: "kb/observations/child.md", Domain: []string{"auth"}, Origin: "authored",
			LineageRefs: localFactRefPaths(raw, "")},
	}
	require.Empty(t, withoutID[1].LineageRefs,
		"an empty repo id reads every stored ref as foreign — this is the trap being pinned")
	_, found = containsToken(
		enumerateBridgeCandidates(withoutID, clusters, BridgeDomain, ScopeFilter{}), "auth")
	require.True(t, found,
		"with an empty repo id the SAME lineage pair sails through — if this ever goes false, "+
			"normalization has moved and the empty-id hazard is gone; delete this test rather than weaken it")
}

// TestBridgeLineage_BareRefSpellingAlsoExcludes covers the other spelling a ref
// can legitimately carry: a schemeless repo-relative path. Both spellings mean
// the same citation, so both must exclude.
func TestBridgeLineage_BareRefSpellingAlsoExcludes(t *testing.T) {
	parent := lineageSeed("kb/synthesis/parent.md")
	child := lineageSeed("kb/observations/child.md", "kb/synthesis/parent.md")

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child},
		twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md"),
		BridgeDomain, ScopeFilter{})

	_, found := containsToken(got, "auth")
	require.False(t, found, "a schemeless ref names the same fact and must exclude too: %+v", got)
}

// ── greedy drop, and what the drop does to the span ──

// TestBridgeLineage_DropsOnlyTheLineageMember is the issue's 5-seed shape: a
// synthesis seeded beside its own source AND an unrelated fact. The unrelated
// member is a legitimate bridge partner and must survive; only the colliding
// member goes.
//
// WHICH member goes is pinned here because a later test depends on it: the
// greedy pass walks members in PATH ORDER and keeps the first of any colliding
// pair, exactly as disjointMembers does. So the path-first member survives and
// the one that cites it is dropped — deterministic, and deliberately not a
// judgement about which of a parent and a child is worth more.
func TestBridgeLineage_DropsOnlyTheLineageMember(t *testing.T) {
	parent := lineageSeed("kb/a-synthesis.md")
	child := lineageSeed("kb/b-child.md", storedRef("kb/a-synthesis.md"))
	stranger := lineageSeed("kb/c-stranger.md")

	// Three communities, so the survivors still span two after the drop.
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/a-synthesis.md"}, 1: {"kb/b-child.md"}, 2: {"kb/c-stranger.md"},
	}}

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child, stranger}, clusters, BridgeDomain, ScopeFilter{})

	set, found := containsToken(got, "auth")
	require.True(t, found, "the group must survive with its non-lineage members: %+v", got)
	require.Len(t, set.Members, 2, "exactly one member — the colliding one — is dropped")
	require.True(t, setHasMember(set, "kb/a-synthesis.md"),
		"path-first member survives the collision")
	require.False(t, setHasMember(set, "kb/b-child.md"),
		"the member that cites an already-kept member is the one dropped")
	require.True(t, setHasMember(set, "kb/c-stranger.md"),
		"an unrelated member is not collateral damage")
}

// TestBridgeLineage_CollapsedSpanDropsTheGroup pins the ORDER of the gates:
// the lineage drop runs BEFORE the community-span check. The dropped member is
// the only thing that made this group cross-community, so once it leaves both
// survivors sit in one community and there is no bridge left.
//
// Under the reversed order (span first) this group would be emitted, because
// the span was still 2 when it was measured. That is the sabotage this test
// exists to catch, and no other test in this file catches it: every other
// exclusion case collapses to fewer than two members instead, which the
// len(members) < 2 guard would reject under either ordering.
func TestBridgeLineage_CollapsedSpanDropsTheGroup(t *testing.T) {
	parent := lineageSeed("kb/a-parent.md")
	child := lineageSeed("kb/b-child.md", storedRef("kb/a-parent.md"))
	sibling := lineageSeed("kb/c-sibling.md")

	// The CHILD — the member the greedy pass drops — is alone in community 1.
	// Both survivors sit in community 0.
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/a-parent.md", "kb/c-sibling.md"},
		1: {"kb/b-child.md"},
	}}

	// Fixture check: before the drop this group really does span two
	// communities, so the span check it must not reach would have passed.
	require.Len(t, []factForLLM{parent, child, sibling}, 3)

	got := enumerateBridgeCandidates(
		[]factForLLM{parent, child, sibling}, clusters, BridgeDomain, ScopeFilter{})

	_, found := containsToken(got, "auth")
	require.False(t, found,
		"the span was only 2 BECAUSE of the dropped member; after the drop the "+
			"survivors are in one community and this is not a bridge: %+v", got)
}

// ── the motif axis ──

// TestBridgeLineage_MotifSignalExcludesParentAndOwnSource is s175: the shared-
// motif signal's first live firing offered a parent and its own source as a
// reason to synthesize, because the parent had INHERITED the child's motif. A
// motif shared BY CITATION is not a recurrence, and the signal cannot be
// trusted until such pairs are gone.
func TestBridgeLineage_MotifSignalExcludesParentAndOwnSource(t *testing.T) {
	clusters := twoCommunities("kb/synthesis/parent.md", "kb/observations/child.md")
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 3})

	control := []factForLLM{
		{File: "kb/synthesis/parent.md", Motifs: []string{"legitimate-tool-hides-intent"},
			Entities: []string{"Alpha"}},
		{File: "kb/observations/child.md", Motifs: []string{"legitimate-tool-hides-intent"},
			Entities: []string{"Beta"}},
	}
	got, _ := enumerateMotifCandidates(control, clusters, identityResolver, constDF(2), labels, tierExact)
	require.Len(t, got, 1,
		"control: two independently-assigned carriers of one motif ARE a candidate")

	lineage := []factForLLM{
		control[0],
		{File: "kb/observations/child.md", Motifs: []string{"legitimate-tool-hides-intent"},
			Entities: []string{"Beta"},
			LineageRefs: localFactRefPaths(
				[]string{storedRef("kb/synthesis/parent.md")}, testLocalRepoID)},
	}
	got, _ = enumerateMotifCandidates(lineage, clusters, identityResolver, constDF(2), labels, tierExact)
	require.Empty(t, got,
		"the SAME pair, with the child citing the parent, is a motif manufactured by "+
			"citation and must not be enumerated: %+v", got)
}

// ── the scoped (token-optional) path ──

// TestBridgeLineage_FilteredPathExcludesLineagePair covers the third
// enumeration path — the one scoped sessions use. It does not share
// enumerateBridgeCandidates with the other two, so an exclusion that only
// landed there would leave every scoped session unprotected.
func TestBridgeLineage_FilteredPathExcludesLineagePair(t *testing.T) {
	run := func(t *testing.T, childRefs []string) []BridgeSeedSet {
		t.Helper()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		seeds := []factForLLM{
			{File: "a.md", Domain: []string{"billing"}, Entities: []string{"Invoice"}},
			{File: "b.md", Domain: []string{"billing"}, Entities: []string{"Payment"},
				LineageRefs: localFactRefPaths(childRefs, testLocalRepoID)},
		}
		g := store.NewSimilarityGraph([][2]string{{"a.md", "b.md"}})
		clusters := ClusterResult{Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}}}

		idx := NewMockSearchIndex(ctrl)
		idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.InAnyOrder([]string{"a.md", "b.md"})).
			Return(g, nil).AnyTimes()
		idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
			Return(map[string]struct{}{}, nil).AnyTimes()

		out, err := buildFilteredBridges(context.Background(), idx, "main", seeds, clusters,
			ScopeFilter{Domain: []string{"auth"}}, EffortHigh, filteredCfg)
		require.NoError(t, err)
		return out
	}

	require.Len(t, run(t, nil), 1,
		"control: the scoped extractor DOES produce a bridge for this pair when nothing cites")
	require.Empty(t, run(t, []string{storedRef("a.md")}),
		"the same pair, with b citing a, must not reach an agent")
}

// TestBridgeLineage_TransitiveIsDELIBERATELYNotExcluded records the boundary of
// this fix so a later reader does not file the gap as a bug.
//
// A grandparent/grandchild pair — a cites b, b cites c, nothing links a and c —
// still bridges. Excluding it needs a refs index or a graph walk, and the
// ruling (2026-08-26) defers building either until a drain shows grandparent
// loops actually occurring. Deeper lineage is still FELT, at rank, through
// derivationGap's DERIVED_FROM term.
//
// If a future change makes this pair excluded, this test failing is the correct
// signal: the deferral was lifted, and that is a decision worth being told about.
func TestBridgeLineage_TransitiveIsDELIBERATELYNotExcluded(t *testing.T) {
	a := lineageSeed("kb/a-top.md", storedRef("kb/b-mid.md"))
	b := lineageSeed("kb/b-mid.md", storedRef("kb/c-bottom.md"))
	c := lineageSeed("kb/c-bottom.md")

	// Only a and c are offered, so nothing in hand links them.
	got := enumerateBridgeCandidates(
		[]factForLLM{a, c},
		twoCommunities("kb/a-top.md", "kb/c-bottom.md"),
		BridgeDomain, ScopeFilter{})

	set, found := containsToken(got, "auth")
	require.True(t, found, "one-hop only: a grandparent pair is NOT excluded today")
	require.Len(t, set.Members, 2)
	require.NotEmpty(t, b.LineageRefs, "fixture check: the middle fact really does carry the hop")
}

// ── the WIRING: the projection sites, not the gate ──
//
// Every test above builds factForLLM by hand and therefore asserts that the
// GATE works. None of them asserts that anything POPULATES LineageRefs
// correctly, which is the half that actually meets production spelling — and a
// sabotage replacing localFactRefPaths(f.Refs, id) with a bare f.Refs at the
// projection sites passed all of them (the #117a shape: a test that calls the
// helper is not a test of the wiring). The two below drive the real projections.

// TestBridgeLineage_ReviewProjectionNormalizesStoredRefs pins the forward
// (review) projection: a fact.Fact carrying a ref in the CANONICAL STORED
// spelling must arrive at the scorer as the bare local path the gate compares.
func TestBridgeLineage_ReviewProjectionNormalizesStoredRefs(t *testing.T) {
	parentPath := "kb/synthesis/parent.md"

	parent := fact.NewFact(parentPath)
	parent.Title, parent.Body, parent.Type = "parent", "parent body", fact.Synthesis
	parent.Domain = []string{"auth"}

	child := fact.NewFact("kb/observations/child.md")
	child.Title, child.Body, child.Type = "child", "child body", fact.Observation
	child.Domain = []string{"auth"}
	child.Refs = []string{storedRef(parentPath)}

	// Fixture check: the ref really is in the kb:// stored form, not a bare
	// path — otherwise this test would pass with normalization removed.
	require.Contains(t, child.Refs[0], "kb://",
		"fixture must use the stored spelling; a bare path would not exercise normalization")

	seeds := factsForLLM([]fact.Fact{parent, child}, testLocalRepoID)
	require.Len(t, seeds, 2)

	var childSeed factForLLM
	for _, s := range seeds {
		if s.File == child.Path() {
			childSeed = s
		}
	}
	require.Equal(t, []string{parentPath}, childSeed.LineageRefs,
		"the projection must reduce the stored kb:// ref to the local path the gate compares")

	// And end to end through the gate, so the wiring is pinned by behaviour and
	// not only by a field value.
	_, found := containsToken(
		enumerateBridgeCandidates(seeds, twoCommunities(parent.Path(), child.Path()),
			BridgeDomain, ScopeFilter{}),
		"auth")
	require.False(t, found, "projected seeds must carry enough for the gate to fire")
}

// TestBridgeLineage_BackwardProjectionExcludesLineagePair pins the SECOND
// projection — the hypothesize path's own factForLLM construction, which does
// not go through factsForLLM and so could regress independently.
func TestBridgeLineage_BackwardProjectionExcludesLineagePair(t *testing.T) {
	ctx := context.Background()

	mk := func(path string, refs ...string) fact.Fact {
		f := fact.NewFact(path)
		f.Title, f.Body, f.Type = path, path, fact.Synthesis
		f.Domain, f.Entities = []string{"auth"}, []string{"shared"}
		f.Refs = refs
		return f
	}

	run := func(t *testing.T, facts []fact.Fact) []BridgeSeedSet {
		t.Helper()
		m := NewMockSearchIndex(gomock.NewController(t))
		expectScopedClusterPartition(m, nil, [][]string{{"kb/a.md"}, {"kb/b.md"}})
		m.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
				pairs := make([][2]string, 0)
				for i := 0; i < len(paths); i++ {
					for j := i + 1; j < len(paths); j++ {
						pairs = append(pairs, [2]string{paths[i], paths[j]})
					}
				}
				return store.NewSimilarityGraph(pairs), nil
			}).AnyTimes()
		m.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
			Return(map[string]struct{}{}, nil).AnyTimes()
		m.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(1, nil).AnyTimes()

		out, err := BuildBackwardBridges(ctx, m, facts, "agent/test", testLocalRepoID,
			EffortHigh, BridgeDomain, 2.0, 2, testCfg, ScopeFilter{})
		require.NoError(t, err)
		return out
	}

	control := run(t, []fact.Fact{mk("kb/a.md"), mk("kb/b.md")})
	_, found := containsToken(control, "auth")
	require.True(t, found, "control: the backward path DOES bridge this pair when nothing cites")

	lineage := run(t, []fact.Fact{mk("kb/a.md"), mk("kb/b.md", storedRef("kb/a.md"))})
	_, found = containsToken(lineage, "auth")
	require.False(t, found,
		"the same pair, with b citing a in the stored spelling, must not reach an agent")
}
