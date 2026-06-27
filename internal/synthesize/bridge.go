package synthesize

import (
	"context"
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
)

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

// maxBridgeSeeds is an absolute ceiling on bridge seed sets, applied in EVERY
// direction as a defensive backstop behind the per-effort budget. effortBudget
// already caps every pool at 48 (high) or 12 (medium), comfortably below this
// ceiling, so it normally never triggers. It exists to guard the forward
// priority-band invariant if effortBudget is ever raised: forward discover
// items get priority forwardDiscoverPriorityBase - rank; with at most
// maxBridgeSeeds items the largest rank is maxBridgeSeeds-1, so the lowest
// priority is forwardDiscoverPriorityBase-(maxBridgeSeeds-1) = reflectPriority+1
// — still strictly above reflect, never colliding.
//
// Derived from the band width so it can't drift if either bound moves.
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
	// Strength is a deterministic ranking signal: token rarity in the corpus
	// (1 / freq) × number of distinct communities × member count. Higher =
	// the bridge spans more across the corpus AND links rarer concepts.
	Strength float64 `json:"-"`
}

// bridgeSeeds returns ranked BridgeSeedSets for the given seed pool and
// cluster assignment.
//
//   - seeds is the candidate fact pool (typically the dirtyFacts/synthesis pool
//     after scope filtering). Facts with origin=discovered are ALWAYS excluded
//     from seed sets — Plan 03 §7 idempotency: discovery never feeds on its own
//     output.
//   - clusters supplies the path → community-id map; facts whose path has no
//     cluster assignment (noise) are still eligible if they share a token with
//     a clustered fact in a different community.
//   - kind controls which structural tokens are considered (domain / entity /
//     both).
//   - eff bounds the result: it truncates to effortBudget(eff) (12 at medium,
//     48 at high) for scoped and unscoped pools alike, so the dial governs
//     breadth either way. The absolute maxBridgeSeeds backstop applies on top.
//     EffortNormal returns nil — the discovery engine never engages.
//
// Bridge definition: a shared token T appears on ≥2 facts whose communities
// differ. Members may include all facts carrying T (they are short cohesive
// groups, not just pairs) — the cross-cluster requirement applies to the SET,
// not every pairwise edge.
func bridgeSeeds(seeds []factForLLM, clusters store.ClusterResult, kind BridgeKind, eff Effort) []BridgeSeedSet {
	if !eff.Discovers() {
		return nil
	}
	if kind == "" {
		kind = DefaultBridgeKind
	}

	// path → community id; noise paths are absent (treated as their own
	// "noise" community for cross-cluster purposes — i.e. linking a noise
	// fact to a clustered fact crosses a community boundary, which is good).
	pathCom := map[string]int{}
	for cid, paths := range clusters.Clusters {
		for _, p := range paths {
			pathCom[p] = cid
		}
	}
	// Noise paths get a synthetic community id so each one is its own
	// "cluster" — bridging two noise facts via a shared token still spans
	// communities, which matches the bridge-is-cross-community semantic.
	nextNoise := -1
	for _, p := range clusters.Noise {
		if _, ok := pathCom[p]; ok {
			continue
		}
		pathCom[p] = nextNoise
		nextNoise--
	}
	// Seeds present in neither a cluster nor the noise list (e.g. dropped
	// upstream by small-cluster filtering or dedup) would otherwise collapse
	// to the map zero value 0 and collide with genuine community id 0 —
	// silently masking real cross-cluster bridges (and conflating two orphans
	// as same-community). Give each orphan its own synthetic community id, the
	// same way noise paths are handled above.
	for _, f := range seeds {
		if _, ok := pathCom[f.File]; ok {
			continue
		}
		pathCom[f.File] = nextNoise
		nextNoise--
	}

	// Token frequency across the seed pool (used for rarity weighting).
	tokenFreq := map[string]int{}
	// tokenKind: entity beats domain when both axes carry the same token.
	// Set only if not already set so the entity loop (runs first) wins.
	tokenKind := map[string]BridgeKind{}

	// path → fact for fast lookup once we know which tokens to follow.
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
				tokenFreq[e]++
				if _, already := tokenKind[e]; !already {
					tokenKind[e] = BridgeEntity
				}
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d == "" {
					continue
				}
				tokenFreq[d]++
				if _, already := tokenKind[d]; !already {
					tokenKind[d] = BridgeDomain
				}
			}
		}
	}

	// For each token, collect carrying facts and check community span.
	// Use a per-token path-set to deduplicate facts that carry the same
	// token on both their Domain and Entities fields.
	tokenMembersByPath := map[string]map[string]factForLLM{}
	for _, f := range byPath {
		addMember := func(token string) {
			if tokenMembersByPath[token] == nil {
				tokenMembersByPath[token] = make(map[string]factForLLM)
			}
			tokenMembersByPath[token][f.File] = f
		}
		if kind == BridgeEntity || kind == BridgeBoth {
			for _, e := range f.Entities {
				if e != "" {
					addMember(e)
				}
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d != "" {
					addMember(d)
				}
			}
		}
	}
	// Flatten to slices for the bridge-filtering step below.
	tokenMembers := make(map[string][]factForLLM, len(tokenMembersByPath))
	for token, pathMap := range tokenMembersByPath {
		members := make([]factForLLM, 0, len(pathMap))
		for _, f := range pathMap {
			members = append(members, f)
		}
		tokenMembers[token] = members
	}

	var out []BridgeSeedSet
	for token, members := range tokenMembers {
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
		freq := tokenFreq[token]
		if freq < 1 {
			freq = 1
		}
		strength := (1.0 / float64(freq)) * float64(len(coms)) * float64(len(members))
		// Deterministic member order (path-sorted) so seed sets are
		// stable across runs and across map-iteration orderings.
		sort.SliceStable(members, func(i, j int) bool { return members[i].File < members[j].File })
		out = append(out, BridgeSeedSet{
			Token:    token,
			Kind:     tokenKind[token],
			Members:  members,
			Strength: strength,
		})
	}

	// Rank by strength desc, with token name as deterministic tiebreaker.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Strength != out[j].Strength {
			return out[i].Strength > out[j].Strength
		}
		return out[i].Token < out[j].Token
	})

	budget := effortBudget(eff)
	if budget > 0 && len(out) > budget {
		out = out[:budget]
	}
	// Absolute backstop behind the per-effort budget: bounds per-bridge LLM work
	// and keeps the forward priority band from overflowing into reflect (see
	// maxBridgeSeeds). Normally inert — effortBudget already caps below it.
	if len(out) > maxBridgeSeeds {
		out = out[:maxBridgeSeeds]
	}
	return out
}

// BuildBackwardBridges is the public entry for the hypothesize pipeline. It
// takes a synthesis-fact pool, builds a ClusterResult from those facts'
// communities via the cached cluster cache, and returns ranked bridges
// (cross-cluster shared tokens).
//
// The caller is responsible for scope-filtering synthFacts before this call;
// the bridge engine truncates the result by effort budget regardless of whether
// a scope filter was applied (effort governs breadth in both cases).
//
// resolution / minCommunitySize are the Louvain parameters the caller pulls
// from the per-repo cluster config (ri.ClusterResolution() /
// ri.ClusterMinCommunitySize()) — the SAME knob the forward (review) path
// clusters with. Passing them in (rather than hardcoding) keeps both discovery
// directions on one community partition and reuses the warm cache the
// background cluster checker maintains, instead of forcing a private,
// never-refreshed cache entry. The cluster cache is keyed by
// (branch, resolution, min_community_size); matching the forward path's
// parameters means both see identical membership.
func BuildBackwardBridges(
	ctx context.Context,
	idx store.SearchIndex,
	synthFacts []fact.Fact,
	branch string,
	effort Effort,
	kind BridgeKind,
	resolution float64,
	minCommunitySize int,
) ([]BridgeSeedSet, error) {
	if !effort.Discovers() || len(synthFacts) < 2 {
		return nil, nil
	}
	// Reduce synthFacts to factForLLM with origin so the idempotency filter
	// can run.
	seeds := make([]factForLLM, 0, len(synthFacts))
	for _, f := range synthFacts {
		seeds = append(seeds, factForLLM{
			File:       f.Path(),
			Title:      f.Title,
			Body:       f.Body,
			Type:       string(f.Type),
			Domain:     f.Domain,
			Entities:   f.Entities,
			Confidence: f.Confidence,
			Sources:    f.Sources,
			Origin:     string(f.Origin),
		})
	}
	cr, err := idx.CachedClusterFacts(ctx, branch, resolution, minCommunitySize)
	if err != nil {
		return nil, err
	}
	return bridgeSeeds(seeds, cr, kind, effort), nil
}
