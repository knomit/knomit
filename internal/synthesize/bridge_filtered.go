package synthesize

import (
	"context"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// scopeLabel returns a human-readable label for the scope filter. The label is
// the Domain and Entities tokens joined with ", " in declaration order
// (Domain first, then Entities). Returns "" when the scope is empty (unscoped).
//
// Declaration order is preserved (not sorted) so the label is stable across
// calls with the same ScopeFilter value — callers that need canonical ordering
// must sort before passing.
func scopeLabel(s ScopeFilter) string {
	if s.IsEmpty() {
		return ""
	}
	parts := make([]string, 0, len(s.Domain)+len(s.Entities))
	parts = append(parts, s.Domain...)
	parts = append(parts, s.Entities...)
	return strings.Join(parts, ", ")
}

// ScopeLabel is the exported form of scopeLabel. It is used by callers outside
// the synthesize package (e.g. the MCP hypothesize handler) that need to set
// DiscoverWorkPayload.ScopeLabel for backward discover items.
func ScopeLabel(s ScopeFilter) string {
	return scopeLabel(s)
}

// neutralSpec is the within-scope specificity returned when bridge members
// share no common sub-token. It signals to the caller (the filtered-bridge
// scorer) that specificity is unknown and ranking must lean on cohesion,
// separation, and derivation-gap instead.
const neutralSpec = 0.0

// factCarriesToken reports whether fact f carries token under the matching
// rules for kind:
//   - BridgeDomain: any Domain entry matches via store.DomainTagMatches
//     (slash-hierarchy / token-containment, canonical).
//   - BridgeEntity: any Entities entry matches via store.EntityTagMatches
//     (case-fold equality).
//   - BridgeBoth (or ""): either axis matches.
func factCarriesToken(f factForLLM, token string, kind BridgeKind) bool {
	if kind == "" {
		kind = DefaultBridgeKind
	}
	if kind == BridgeEntity || kind == BridgeBoth {
		for _, e := range f.Entities {
			if store.EntityTagMatches(e, token) {
				return true
			}
		}
	}
	if kind == BridgeDomain || kind == BridgeBoth {
		for _, d := range f.Domain {
			if store.DomainTagMatches(d, token) {
				return true
			}
		}
	}
	return false
}

// specificityWithinScope returns 1/max(df,1) where df is the number of pool
// facts that carry token under the given kind rules. This is an in-memory,
// within-scope document frequency — it operates over the provided []factForLLM
// pool, not over the branch-wide TokenDF index.
//
// Examples:
//
//	df=0 → 1.0   (absent token is maximally specific)
//	df=1 → 1.0
//	df=4 → 0.25
func specificityWithinScope(token string, kind BridgeKind, pool []factForLLM) float64 {
	df := 0
	for _, f := range pool {
		if factCarriesToken(f, token, kind) {
			df++
		}
	}
	if df < 1 {
		df = 1
	}
	return 1.0 / float64(df)
}

// tokenAxis bundles a canonical token key with its kind and first-seen
// authored form, used internally by sharedSubToken.
type tokenAxis struct {
	canon    string
	kind     BridgeKind
	authored string
}

// tokensCarriedBy enumerates all tokens carried by fact f under kind,
// returning them in entity-first order (entity beats domain, mirroring
// enumerateBridgeCandidates). Each canonical form is returned at most once;
// if the same canonical form appears on both axes, the entity kind wins.
func tokensCarriedBy(f factForLLM, kind BridgeKind) []tokenAxis {
	if kind == "" {
		kind = DefaultBridgeKind
	}
	seen := map[string]bool{}
	var out []tokenAxis

	if kind == BridgeEntity || kind == BridgeBoth {
		for _, e := range f.Entities {
			if e == "" {
				continue
			}
			c := store.CanonicalizeTag(e)
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, tokenAxis{canon: c, kind: BridgeEntity, authored: e})
		}
	}
	if kind == BridgeDomain || kind == BridgeBoth {
		for _, d := range f.Domain {
			if d == "" {
				continue
			}
			c := store.CanonicalizeTag(d)
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, tokenAxis{canon: c, kind: BridgeDomain, authored: d})
		}
	}
	return out
}

// sharedSubToken finds the canonical token carried by ALL members, picks the
// rarest one in pool (highest within-scope idf = lowest df), and returns its
// first-seen authored form, its kind, and its within-scope specificity.
//
// Kind rules (BridgeBoth → both axes; entity beats domain on kind when a
// token appears as both, mirroring enumerateBridgeCandidates):
//   - BridgeDomain: only Domain entries considered.
//   - BridgeEntity: only Entities entries considered.
//   - BridgeBoth (or ""): both axes; entity kind wins over domain for the
//     same canonical token.
//
// Tie-break when two shared tokens have equal df in pool: smallest canonical
// string wins (deterministic).
//
// Edge cases:
//   - len(members) < 2: returns ("", BridgeBoth, neutralSpec). The filtered
//     generator only calls this for subsets of size ≥ 2 in practice, but the
//     function is safe for smaller inputs.
//   - No shared token: returns ("", BridgeBoth, neutralSpec).
func sharedSubToken(members []factForLLM, kind BridgeKind, pool []factForLLM) (token string, tkind BridgeKind, spec float64) {
	if kind == "" {
		kind = DefaultBridgeKind
	}
	if len(members) < 2 {
		return "", BridgeBoth, neutralSpec
	}

	// Build the set of canonical tokens carried by the first member.
	// authored maps canonical → first-seen authored form.
	// kindOf maps canonical → effective BridgeKind (entity beats domain).
	type entry struct {
		authored string
		kind     BridgeKind
	}
	shared := map[string]entry{}
	for _, ta := range tokensCarriedBy(members[0], kind) {
		shared[ta.canon] = entry{authored: ta.authored, kind: ta.kind}
	}

	// Intersect with every subsequent member.
	for _, m := range members[1:] {
		if len(shared) == 0 {
			break
		}
		carried := tokensCarriedBy(m, kind)
		carriedSet := map[string]bool{}
		for _, ta := range carried {
			carriedSet[ta.canon] = true
		}
		for c := range shared {
			if !carriedSet[c] {
				delete(shared, c)
			}
		}
	}

	if len(shared) == 0 {
		return "", BridgeBoth, neutralSpec
	}

	// Among shared tokens, pick the one with the lowest df in pool
	// (rarest = most specific). Tie-break: smallest canonical string.
	bestCanon := ""
	bestDF := -1
	bestAuthored := ""
	bestKind := BridgeBoth

	for c, e := range shared {
		df := 0
		for _, f := range pool {
			if factCarriesToken(f, e.authored, e.kind) {
				df++
			}
		}
		better := bestCanon == "" ||
			df < bestDF ||
			(df == bestDF && c < bestCanon)
		if better {
			bestCanon = c
			bestDF = df
			bestAuthored = e.authored
			bestKind = e.kind
		}
	}

	return bestAuthored, bestKind, specificityWithinScope(bestAuthored, bestKind, pool)
}

// buildFilteredBridges extracts multiple token-optional cohesive cross-community
// seed sets directly from the SIMILAR_TO graph over a scoped pool, reusing the
// Phase-4 reshapeCohesiveSubset seed-and-grow.
//
// Pipeline:
//  1. Gate: if !eff.Discovers() return (nil, nil) — no idx calls.
//  2. Drop §7 discovered-origin seeds; collect sorted non-discovered paths and
//     byPath lookup. If < 2 paths → return nil, nil.
//  3. Build clusterOf via bridgePathCommunities; fetch the full SimilarityGraph
//     once for all paths.
//  4. Iterative extraction: pull the best cross-community cohesive subset via
//     reshapeCohesiveSubset; process it; remove its members from remaining;
//     repeat until no seam is found. Guard: if reshape returns a subset that
//     does not shrink remaining, break to prevent infinite loops.
//  5. For each subset: label via sharedSubToken; if the shared token canonically
//     matches the scope, demote to no-token (tok="", spec=neutralSpec). Score,
//     gate, and append.
//  6. Rank by Q desc; tie-break Token asc then first-member-path asc; cap to
//     effortBudget(eff).
//
// The branch parameter is accepted for call-site symmetry with
// buildScoredBridges (Task 21 dispatches between the two) but is intentionally
// unused here: the filtered path scores specificity within-scope via
// sharedSubToken, never via idx.TokenDF(branch, ...).
func buildFilteredBridges(
	ctx context.Context,
	idx store.SearchIndex,
	branch string,
	seeds []factForLLM,
	clusters store.ClusterResult,
	scope ScopeFilter,
	eff Effort,
	cfg QualityConfig,
) ([]BridgeSeedSet, error) {
	// §1 gate — must be first, before any idx call.
	if !eff.Discovers() {
		return nil, nil
	}

	// §2 — build non-discovered pool.
	byPath := make(map[string]factForLLM, len(seeds))
	for _, f := range seeds {
		if f.Origin == string(fact.Discovered) {
			continue
		}
		byPath[f.File] = f
	}
	if len(byPath) < 2 {
		return nil, nil
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// §3 — community map and similarity graph.
	clusterOf := bridgePathCommunities(seeds, clusters)
	g, err := idx.SimilarityAdjacency(ctx, paths)
	if err != nil {
		return nil, err
	}

	// §4 — iterative extraction.
	remaining := append([]string{}, paths...)
	var out []BridgeSeedSet
	for len(remaining) >= 2 {
		sub := reshapeCohesiveSubset(remaining, g, clusterOf, cfg.CohFloor, cfg.MaxMembers)
		if sub == nil {
			break
		}
		// Progress guard: reshapeCohesiveSubset only ever returns members of its
		// input, so removing sub from remaining always shrinks it. The single
		// degenerate case is an empty (but non-nil) subset, which would leave
		// remaining unchanged → infinite loop; break defensively so the loop
		// always makes progress.
		if len(sub) == 0 {
			break
		}

		// §5 — label: find shared sub-token; demote scope token to "".
		members := make([]factForLLM, 0, len(sub))
		for _, p := range sub { // sub is already path-sorted by reshapeCohesiveSubset
			if f, ok := byPath[p]; ok {
				members = append(members, f)
			}
		}

		tok, tkind, spec := sharedSubToken(members, DefaultBridgeKind, seeds)
		if tok != "" {
			// Check whether tok canonically matches the scope along its own axis
			// (by-kind rule matching enumerateBridgeCandidates Item-2).
			scopeMatch := false
			if tkind == BridgeDomain {
				for _, sd := range scope.Domain {
					if store.DomainTagMatches(tok, sd) {
						scopeMatch = true
						break
					}
				}
			} else if tkind == BridgeEntity {
				for _, se := range scope.Entities {
					if store.EntityTagMatches(tok, se) {
						scopeMatch = true
						break
					}
				}
			}
			if scopeMatch {
				tok = ""
				spec = neutralSpec
			}
		}

		gap, err := derivationGap(ctx, sub, idx)
		if err != nil {
			return nil, err
		}
		comp := BridgeComponents{
			Coh:     cohesion(sub, g),
			Sep:     separation(sub, clusterOf),
			Gap:     gap,
			Spec:    spec,
			Members: len(sub),
		}
		q, kept := bridgeQ(comp, cfg)
		if kept {
			out = append(out, BridgeSeedSet{
				Token:   tok,
				Kind:    tkind,
				Members: members,
				Q:       q,
			})
		}

		// Remove sub members from remaining. Use a zero-cap header so the first
		// append allocates a fresh backing array — no aliasing with the slice
		// being read.
		subSet := make(map[string]bool, len(sub))
		for _, p := range sub {
			subSet[p] = true
		}
		next := remaining[:0:0]
		for _, p := range remaining {
			if !subSet[p] {
				next = append(next, p)
			}
		}
		remaining = next
	}

	if len(out) == 0 {
		return nil, nil
	}

	// §6 — rank Q desc; tie-break Token asc, then first-member-path asc.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Q != out[j].Q {
			return out[i].Q > out[j].Q
		}
		if out[i].Token != out[j].Token {
			return out[i].Token < out[j].Token
		}
		piPath := ""
		if len(out[i].Members) > 0 {
			piPath = out[i].Members[0].File
		}
		pjPath := ""
		if len(out[j].Members) > 0 {
			pjPath = out[j].Members[0].File
		}
		return piPath < pjPath
	})

	// Cap to effort budget.
	budget := effortBudget(eff)
	if budget > 0 && len(out) > budget {
		out = out[:budget]
	}
	return out, nil
}
