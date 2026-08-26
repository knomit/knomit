package synthesize

import (
	"sort"

	"knomit/internal/fact"
)

// One-hop lineage exclusion for bridge candidates (#125).
//
// A bridge is supposed to connect facts that nothing already connects. A
// synthesis inherits its members' domain tags, so a token bridge can always
// reconnect it to the very facts it was distilled from — measured on core
// (2026-08-25): 4 of 4 discover items paired a synthesis with its own sources,
// one of them a 5-seed item holding a synthesis plus THREE of its parents.
// Reinforcing such a group raises `sources` using the evidence the fact was
// already built from, which is a false ledger, not a confirmation.
//
// The exclusion is deliberately ONE HOP in either direction: a pair is dropped
// when one member's refs name the other. Transitive (grandparent and deeper)
// lineage is NOT excluded here and that is a decision, not an omission — it
// would need a refs index or a graph walk, and neither is built speculatively
// (designer ruling 2026-08-26). Deeper lineage is still felt at RANK, by
// derivationGap's DERIVED_FROM term, which is why that term stays.

// canonFactPath renders a fact's own path in the same canonical spelling
// fact.ClassifyRef gives a ref's Path, so a member's path and another member's
// stored refs can be compared as strings.
//
// A member path is always SCHEMELESS — it is the repo-relative path of a fact
// in this repo, never a kb:// wire form — and ClassifyRef's schemeless branch
// ignores localRepoID entirely when computing Path. Passing "" here is
// therefore exact rather than a lossy shortcut; it is the stored REFS, not the
// paths, that need the real repo id (see factForLLM.LineageRefs).
func canonFactPath(p string) string {
	return fact.ClassifyRef(p, "").Path
}

// lineagePair reports whether a and b stand in a direct citation relation —
// b's path is among a's local refs, or a's among b's. Either direction counts:
// a parent seeded beside its child is the same circularity as a child seeded
// beside its parent.
func lineagePair(a, b factForLLM) bool {
	return cites(a, b) || cites(b, a)
}

// cites reports whether src's local refs name dst's path.
func cites(src, dst factForLLM) bool {
	want := canonFactPath(dst.File)
	if want == "" {
		return false
	}
	for _, r := range src.LineageRefs {
		if r == want {
			return true
		}
	}
	return false
}

// lineageDisjointMembers applies the one-hop lineage exclusion pairwise,
// keeping a member only when it is lineage-disjoint from every member already
// kept. It returns the survivors and how many members it dropped.
//
// Greedy rather than all-or-nothing, mirroring disjointMembers: one parent
// sitting among four otherwise-unrelated facts must cost the group that one
// member, not the whole bridge. Members are path-sorted first, so which member
// survives a collision is deterministic rather than a function of map
// iteration order.
//
// Callers must run this BEFORE any separation (community-span) check: dropping
// a member can collapse the span, and a group that only spanned two
// communities via the parent it just lost is not a bridge.
func lineageDisjointMembers(members []factForLLM) ([]factForLLM, int) {
	if len(members) < 2 {
		return members, 0
	}
	ordered := make([]factForLLM, len(members))
	copy(ordered, members)
	sortByPath(ordered)

	kept := make([]factForLLM, 0, len(ordered))
	for _, cand := range ordered {
		ok := true
		for _, k := range kept {
			if lineagePair(cand, k) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, cand)
		}
	}
	return kept, len(ordered) - len(kept)
}

// sortByPath orders members by their fact path, in place. Every gate in the
// bridge engine that drops members greedily needs the same deterministic
// starting order, or which member survives a collision becomes a function of
// map iteration order.
func sortByPath(in []factForLLM) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].File < in[j].File })
}

// lineageDisjointMembersMap is lineageDisjointMembers over the path→fact map
// shape the motif enumerator carries its groups in, returning the same shape so
// the gates downstream of it are unchanged.
func lineageDisjointMembersMap(members map[string]factForLLM) map[string]factForLLM {
	if len(members) < 2 {
		return members
	}
	all := make([]factForLLM, 0, len(members))
	for _, f := range members {
		all = append(all, f)
	}
	kept, dropped := lineageDisjointMembers(all)
	if dropped == 0 {
		return members
	}
	out := make(map[string]factForLLM, len(kept))
	for _, f := range kept {
		out[f.File] = f
	}
	return out
}
