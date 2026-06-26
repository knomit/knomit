package synthesize

import (
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

// effortBudget is the maximum number of bridge seed sets the unscoped pool
// is truncated to per effort level. A scoped (filtered) pool is already
// bounded by the agent's request; no truncation is applied.
func effortBudget(e Effort) int {
	switch e {
	case EffortMedium:
		return 12
	case EffortHigh:
		return 48
	}
	return 0
}

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
//   - eff bounds the result: scoped=true returns every bridge ordered by
//     strength (the pool is already small); scoped=false truncates to
//     effortBudget(eff). EffortNormal returns nil — the discovery engine
//     never engages.
//
// Bridge definition: a shared token T appears on ≥2 facts whose communities
// differ. Members may include all facts carrying T (they are short cohesive
// groups, not just pairs) — the cross-cluster requirement applies to the SET,
// not every pairwise edge.
func bridgeSeeds(seeds []factForLLM, clusters store.ClusterResult, kind BridgeKind, eff Effort, scoped bool) []BridgeSeedSet {
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

	// Token frequency across the seed pool (used for rarity weighting).
	tokenFreq := map[string]int{}
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
				tokenKind[e] = BridgeEntity
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d == "" {
					continue
				}
				tokenFreq[d]++
				tokenKind[d] = BridgeDomain
			}
		}
	}

	// For each token, collect carrying facts and check community span.
	tokenMembers := map[string][]factForLLM{}
	for _, f := range byPath {
		if kind == BridgeEntity || kind == BridgeBoth {
			for _, e := range f.Entities {
				if e == "" {
					continue
				}
				tokenMembers[e] = append(tokenMembers[e], f)
			}
		}
		if kind == BridgeDomain || kind == BridgeBoth {
			for _, d := range f.Domain {
				if d == "" {
					continue
				}
				tokenMembers[d] = append(tokenMembers[d], f)
			}
		}
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

	if !scoped {
		budget := effortBudget(eff)
		if budget > 0 && len(out) > budget {
			out = out[:budget]
		}
	}
	return out
}
