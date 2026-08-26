package synthesize

import (
	"context"
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// bridgePathCommunities builds the path→communityID map used by both
// enumerateBridgeCandidates and buildScoredBridges so both always operate on
// the SAME community assignment. The mapping logic:
//   - Clustered paths get their real community id.
//   - Noise paths each get their own unique synthetic negative id (so two
//     noise facts bridging via a shared token still span communities).
//   - Seeds absent from both clusters and noise also get unique synthetic
//     negative ids (prevents collision with real community 0).
func bridgePathCommunities(seeds []factForLLM, clusters ClusterResult) map[string]int {
	pathCom := make(map[string]int)
	for cid, paths := range clusters.Clusters {
		for _, p := range paths {
			pathCom[p] = cid
		}
	}
	nextNoise := -1
	for _, p := range clusters.Noise {
		if _, ok := pathCom[p]; ok {
			continue
		}
		pathCom[p] = nextNoise
		nextNoise--
	}
	for _, f := range seeds {
		if _, ok := pathCom[f.File]; ok {
			continue
		}
		pathCom[f.File] = nextNoise
		nextNoise--
	}
	return pathCom
}

// Bridge seeding (Plan 03 §3b) is a shared engine that both the review (forward
// discovery) and hypothesize (backward discovery) pipelines drive when
// effort >= medium. It surfaces small heterogeneous {A..D} seed sets where the
// member facts share a structural token (entity or domain) but live in
// distinct cluster communities — the "bridge" pattern from the design spec.
//
// The engine is intentionally model-less: the connected MCP agent is the sole
// reasoner. Bridge seeding only ranks and bounds the candidate space.

// BridgeKind controls which structural tokens count as a bridge: domain,
// entity, or both. Defaults to BridgeBoth.
type BridgeKind string

const (
	BridgeDomain BridgeKind = "domain"
	BridgeEntity BridgeKind = "entity"
	BridgeBoth   BridgeKind = "both"
)

// BridgeMotif is the Phase-3 aspect axis: the shared token is a CANONICAL
// MOTIF ID rather than an entity or a domain tag.
//
// Deliberately absent from BridgeKindFromString below. Entity/domain selection
// is a per-repo config knob (discovery.bridge); motif bridging is bound to the
// effort dial instead (§5), so a config value of "motif" must not be able to
// switch the entity and domain axes off.
//
// Enumeration for this kind lives entirely in bridge_motif.go, which is why no
// function in THIS file mentions motifs — this file's mechanicsPaths entry
// stays nil, and the MN6 declaration it makes stays true.
//
// #125 added a second member to that arrangement: BridgeKind.Qualifies, the
// predicate that says only a shared MECHANISM qualifies a bridge, is declared
// in bridge_motif.go rather than beside BridgeKindFromString here. Its body
// names BridgeMotif, and putting it in this file would have turned the nil
// entry above into a one-entry allow-list — eroding a documented property of
// the entity/domain engine to hold a method whose entire subject is the OTHER
// axis. Same rationale as the enumeration split, applied to a method.
const BridgeMotif BridgeKind = "motif"

// DefaultBridgeKind is the historical default — bridge on either axis.
const DefaultBridgeKind = BridgeBoth

// BridgeKindFromString coerces a config string (the per-repo discovery.bridge
// setting) to a known BridgeKind, falling back to DefaultBridgeKind for empty
// or unrecognized values. Single definition shared by the forward (review) and
// backward (hypothesize) pipelines so both honor the same config knob.
func BridgeKindFromString(s string) BridgeKind {
	switch BridgeKind(s) {
	case BridgeDomain, BridgeEntity, BridgeBoth:
		return BridgeKind(s)
	}
	return DefaultBridgeKind
}

// entityAxisRankOnlyLine is the health line every surface that USED to serve
// entity/domain bridges emits in their place.
//
// It exists because "0 bridges" and "this axis no longer qualifies" are the same
// number, and a later auditor reading a session with no discover items has no
// way to tell an axis that found nothing from one that cannot qualify — the
// saturated-vs-broken confusion #147 was re-ruled over, one surface across.
// The sentence names the state, not a count: there is no count to give.
func entityAxisRankOnlyLine() string {
	return "bridge axis: entity/domain is RANK-ONLY — a shared name no longer qualifies a " +
		"bridge, only a shared mechanism does (#125). Zero entity bridges here is the " +
		"designed state, not a stall."
}

// effortBudget is the maximum number of bridge seed sets a pool is truncated
// to per effort level. It applies to scoped and unscoped pools alike so the
// dial governs breadth in both: medium digs a narrow slice, high a wider one
// (design §3b — "effort governs how deep/wide within the bounded pool"). A
// scoped pool is usually small enough that high (48) captures all of it, while
// medium (12) deliberately takes a lighter pass.
func effortBudget(e Effort) int {
	switch e {
	case EffortMedium:
		return 12
	case EffortHigh:
		return 48
	}
	return 0
}

// maxBridgeSeeds is the width of the forward-discover priority band: the most
// bridge seed sets that can be enqueued before the rank-derived priority would
// collide with reflect. Forward discover items get priority
// forwardDiscoverPriorityBase - rank; with at most maxBridgeSeeds items the
// largest rank is maxBridgeSeeds-1, so the lowest priority is
// forwardDiscoverPriorityBase-(maxBridgeSeeds-1) = reflectPriority+1 — still
// strictly above reflect, never colliding.
//
// effortBudget caps every engaged pool at 48 (high) or 12 (medium), comfortably
// below this width, so the band can never overflow today. There is therefore no
// runtime clamp against it — that check could never fire. Instead the headroom
// (effortBudget(EffortHigh) < maxBridgeSeeds) is asserted by
// TestEffortBudget_StaysBelowPriorityBand, which fails loudly the day someone
// raises the budget past the band width.
const maxBridgeSeeds = forwardDiscoverPriorityBase - reflectPriority

// BridgeSeedSet is a small group of related facts plus the structural token
// that bridges them. The discovery prompt presents this set verbatim and asks
// the connected MCP agent whether a consequence (forward) or keystone
// (backward) is entailed.
type BridgeSeedSet struct {
	// Token is the shared structural label that links the members (an entity
	// or domain name).
	Token string `json:"token"`
	// Kind reports whether Token is an entity or a domain.
	Kind BridgeKind `json:"kind"`
	// Members is the heterogeneous fact slice — at least two distinct
	// community ids represented.
	Members []factForLLM `json:"members"`
	// Q is the weighted quality score (cohesion + derivation-gap + specificity)
	// computed by buildScoredBridges. Used for ranking; zero when the
	// candidate was dropped by a gate (never appears in returned slices).
	Q float64 `json:"-"`
}

// enumerateBridgeCandidates performs the grouping step only: it builds the
// community map (via bridgePathCommunities), applies the §7 discovered-origin
// exclusion, groups non-discovered seed facts by canonical token (entity beats
// domain; dedup for facts carrying the same token on both axes), and returns
// all tokens where len(members) >= 2 AND the members span >= 2 distinct
// communities.
//
// It does NOT compute strength, does NOT cap by effort, and does NOT call any
// index — it is pure over the provided seed pool and cluster assignment. The
// returned sets have Members path-sorted and Token set to the first-seen
// display form. Kind defaults to DefaultBridgeKind when the argument is "".
//
// scope optionally excludes tokens that belong to the active scope from
// becoming bridge candidates. A token is excluded when it canonically matches
// the scope along its own axis only:
//   - A domain-kind token is checked against scope.Domain (using
//     store.DomainTagMatches). It is NOT checked against scope.Entities.
//   - An entity-kind token is checked against scope.Entities (using
//     store.EntityTagMatches). It is NOT checked against scope.Domain.
//
// This by-kind rule prevents false positives when an entity name happens to
// share a string with a domain token or vice versa. Empty scope → no
// exclusion (unscoped/production behavior byte-identical).
//
// This is the enumeration engine used by buildScoredBridges (quality/Q path)
// and BridgeComponentReport (calibrate/diagnostic tool).
func enumerateBridgeCandidates(seeds []factForLLM, clusters ClusterResult, kind BridgeKind, scope ScopeFilter) []BridgeSeedSet {
	if kind == "" {
		kind = DefaultBridgeKind
	}

	pathCom := bridgePathCommunities(seeds, clusters)

	// tokenKind: entity beats domain when both axes carry the same token.
	// Set only if not already set so the entity loop (runs first) wins.
	tokenKind := map[string]BridgeKind{}
	// repForm maps canonical token → first authored form seen, for display.
	repForm := map[string]string{}

	// path → fact for fast lookup; excludes §7 discovered facts.
	byPath := make(map[string]factForLLM, len(seeds))
	for _, f := range seeds {
		// §7 idempotency: discovered facts are never seeds.
		if f.Origin == string(fact.Discovered) {
			continue
		}
		byPath[f.File] = f
		if kind == BridgeEntity || kind == BridgeBoth {
			for _, e := range f.Entities {
				if e == "" {
					continue
				}
				canon := store.CanonicalizeTag(e)
				if _, already := tokenKind[canon]; !already {
					tokenKind[canon] = BridgeEntity
					repForm[canon] = e
				}
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d == "" {
					continue
				}
				canon := store.CanonicalizeTag(d)
				if _, already := tokenKind[canon]; !already {
					tokenKind[canon] = BridgeDomain
					repForm[canon] = d
				}
			}
		}
	}

	// For each token, collect carrying facts and check community span.
	// Use a per-token path-set to deduplicate facts that carry the same
	// token on both their Domain and Entities fields. Keys are canonical.
	tokenMembersByPath := map[string]map[string]factForLLM{}
	for _, f := range byPath {
		addMember := func(canon string) {
			if tokenMembersByPath[canon] == nil {
				tokenMembersByPath[canon] = make(map[string]factForLLM)
			}
			tokenMembersByPath[canon][f.File] = f
		}
		if kind == BridgeEntity || kind == BridgeBoth {
			for _, e := range f.Entities {
				if e != "" {
					addMember(store.CanonicalizeTag(e))
				}
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d != "" {
					addMember(store.CanonicalizeTag(d))
				}
			}
		}
	}

	var out []BridgeSeedSet
	for canon, pathMap := range tokenMembersByPath {
		// Scope exclusion (Task 19 Item 2): if the scope is non-empty, exclude
		// tokens that canonically match the active scope along their own axis.
		// Domain-kind tokens are checked only against scope.Domain; entity-kind
		// tokens only against scope.Entities. Cross-axis matching is deliberately
		// skipped to avoid false positives when an entity name shares a string
		// with a domain token or vice versa.
		if !scope.IsEmpty() {
			tk := tokenKind[canon]
			tok := repForm[canon]
			excluded := false
			if tk == BridgeDomain {
				for _, sd := range scope.Domain {
					if store.DomainTagMatches(tok, sd) {
						excluded = true
						break
					}
				}
			} else if tk == BridgeEntity {
				for _, se := range scope.Entities {
					if store.EntityTagMatches(tok, se) {
						excluded = true
						break
					}
				}
			}
			if excluded {
				continue
			}
		}

		members := make([]factForLLM, 0, len(pathMap))
		for _, f := range pathMap {
			members = append(members, f)
		}
		// One-hop lineage exclusion (#125). Applied BEFORE the community-span
		// check because dropping a member can collapse the span: a group whose
		// two communities were "a synthesis" and "the fact it was distilled
		// from" is not a bridge once the parent leaves. See bridge_lineage.go.
		members, _ = lineageDisjointMembers(members)
		if len(members) < 2 {
			continue
		}
		coms := map[int]struct{}{}
		for _, m := range members {
			coms[pathCom[m.File]] = struct{}{}
		}
		if len(coms) < 2 {
			continue // same-cluster — not a bridge
		}
		// Deterministic member order (path-sorted) so seed sets are
		// stable across runs and across map-iteration orderings.
		sort.SliceStable(members, func(i, j int) bool { return members[i].File < members[j].File })
		out = append(out, BridgeSeedSet{
			Token:   repForm[canon],
			Kind:    tokenKind[canon],
			Members: members,
		})
	}
	return out
}

// buildScoredBridges is the Phase-4 production builder. It enumerates bridge
// candidates (via enumerateBridgeCandidates), reshapes oversized groups to
// their most cohesive cross-community subset, scores each surviving candidate
// with the Q-score (cohesion + derivation-gap + specificity), drops those
// below the quality gate, ranks by Q descending (Token asc on ties), and caps
// to effortBudget(eff).
//
// Gate: if !eff.Discovers() returns (nil, nil) immediately without calling idx.
//
// buildScoredBridges sets the Q field on each returned BridgeSeedSet.
func buildScoredBridges(
	ctx context.Context,
	idx SearchQuery,
	branch string,
	seeds []factForLLM,
	clusters ClusterResult,
	kind BridgeKind,
	eff Effort,
	cfg QualityConfig,
	scope ScopeFilter,
) ([]BridgeSeedSet, error) {
	if !eff.Discovers() {
		return nil, nil
	}

	cands := enumerateBridgeCandidates(seeds, clusters, kind, scope)
	if len(cands) == 0 {
		return nil, nil
	}

	// Build the community map consistent with enumeration.
	clusterOf := bridgePathCommunities(seeds, clusters)

	// byPath allows fast reconstruction of factForLLM from a path after reshape.
	// Exclude discovered-origin facts to mirror enumerateBridgeCandidates's §7
	// idempotency exclusion (they never appear in candidates anyway).
	byPath := make(map[string]factForLLM, len(seeds))
	for _, f := range seeds {
		if f.Origin == string(fact.Discovered) {
			continue
		}
		byPath[f.File] = f
	}

	var out []BridgeSeedSet
	for _, cand := range cands {
		paths := make([]string, 0, len(cand.Members))
		for _, m := range cand.Members {
			paths = append(paths, m.File)
		}

		g, err := idx.SimilarityAdjacency(ctx, paths)
		if err != nil {
			return nil, err
		}

		// Reshape oversized groups to the most cohesive cross-community subset.
		if len(paths) > cfg.MaxMembers {
			sub := reshapeCohesiveSubset(paths, g, clusterOf, cfg.CohFloor, cfg.MaxMembers)
			if sub == nil {
				continue // no cohesive cross-community seam — drop candidate
			}
			paths = sub
		}

		_, q, kept, err := scoreBridgeCandidate(ctx, paths, cand.Kind, cand.Token, g, idx, branch, clusterOf, cfg)
		if err != nil {
			return nil, err
		}
		if !kept {
			continue
		}

		// Rebuild factForLLM slice for the surviving paths (path-sorted).
		members := make([]factForLLM, 0, len(paths))
		for _, p := range paths {
			if f, ok := byPath[p]; ok {
				members = append(members, f)
			}
		}
		sort.SliceStable(members, func(i, j int) bool { return members[i].File < members[j].File })

		out = append(out, BridgeSeedSet{
			Token:   cand.Token,
			Kind:    cand.Kind,
			Members: members,
			Q:       q,
		})
	}

	// Rank by Q desc, Token asc for deterministic output.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Q != out[j].Q {
			return out[i].Q > out[j].Q
		}
		return out[i].Token < out[j].Token
	})

	// Cap to effort budget.
	budget := effortBudget(eff)
	if budget > 0 && len(out) > budget {
		out = out[:budget]
	}
	return out, nil
}

// BuildBackwardBridges is the public entry for the hypothesize pipeline. It
// takes a synthesis-fact pool, clusters those facts in-process via ScopedCluster
// (Louvain over idx.SubgraphEdges), and returns ranked bridges (cross-cluster
// shared tokens).
//
// The caller is responsible for scope-filtering synthFacts before this call;
// the bridge engine truncates the result by effort budget regardless of whether
// a scope filter was applied (effort governs breadth in both cases).
//
// resolution / minCommunitySize are the Louvain parameters the caller pulls
// from the per-repo cluster config (ri.ClusterResolution() /
// ri.ClusterMinCommunitySize()) — the SAME knob the forward (review) path
// clusters with. Passing them in (rather than hardcoding) keeps both discovery
// directions on one community partition: backward clusters its synthesis-fact
// pool with ScopedCluster exactly as the forward path clusters its review seeds,
// so both see identical membership for the same inputs.
func BuildBackwardBridges(
	ctx context.Context,
	idx SearchQuery,
	synthFacts []fact.Fact,
	branch string,
	localRepoID string,
	effort Effort,
	kind BridgeKind,
	resolution float64,
	minCommunitySize int,
	cfg QualityConfig,
	scope ScopeFilter,
) ([]BridgeSeedSet, error) {
	if !effort.Discovers() || len(synthFacts) < 2 {
		return nil, nil
	}
	// Reduce synthFacts to factForLLM with origin so the idempotency filter
	// can run.
	seeds := make([]factForLLM, 0, len(synthFacts))
	for _, f := range synthFacts {
		seeds = append(seeds, factForLLM{
			File:        f.Path(),
			Title:       f.Title,
			Body:        f.Body,
			Type:        string(f.Type),
			Domain:      f.Domain,
			Entities:    f.Entities,
			Confidence:  f.Confidence,
			Sources:     f.Sources,
			Origin:      string(f.Origin),
			LineageRefs: localFactRefPaths(f.Refs, localRepoID),
		})
	}
	groups, err := ScopedCluster(ctx, seeds, idx, resolution, minCommunitySize, func(ProgressEvent) {}, branch)
	if err != nil {
		return nil, err
	}
	cr := clusterResultFromGroups(groups)
	// Dispatch: scoped sessions use the token-optional filtered generator;
	// unscoped sessions use the token-anchored scored generator. Mirrors the
	// forward (review) dispatch in review.go StartSession.
	if !scope.IsEmpty() {
		return buildFilteredBridges(ctx, idx, branch, seeds, cr, scope, effort, cfg)
	}
	return buildScoredBridges(ctx, idx, branch, seeds, cr, kind, effort, cfg, scope)
}
