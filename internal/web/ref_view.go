package web

import (
	"strings"

	"knomit/internal/web/hal"
)

// RefView is the on-wire shape for a single entry in a fact's refs array.
// Kind is one of "fact", "broken", "url". Links is populated only for the
// "fact" kind (with a single "target" entry).
type RefView struct {
	Raw   string      `json:"raw"`
	Kind  string      `json:"kind"`
	Links hal.LinkMap `json:"_links,omitempty"`
}

// RefResolver is used by BuildRefViews to decide whether a fact-path ref is
// navigable at the current anchor. Tests provide stub resolvers; production
// uses FactIndex.FactExists via a thin adapter (created in Task 6.3).
type RefResolver interface {
	// Exists returns true when a fact exists at the given path (visible at
	// the caller's anchor — the caller has already resolved the anchor into
	// the resolver, so this is a simple lookup from the callee's view).
	Exists(path string) bool
}

// isExternalRef matches the store-layer definition of an external (non-fact)
// ref: anything that starts with http(s):// OR doesn't end in `.md`. This
// mirrors the storytest assertions TestFollowRef_ExternalURL and
// TestFollowRef_NoMdSuffixIsExternal — keep these in sync with
// test/testenv/follow_ref.go if the store-layer definition evolves.
func isExternalRef(raw string) bool {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return true
	}
	if !strings.HasSuffix(raw, ".md") {
		return true
	}
	return false
}

// BuildRefViews transforms a raw []string of fact refs into structured
// RefView entries, dispatching each to one of three kinds based on its
// form and, for fact-path refs, whether the resolver says the target is
// visible at the current anchor.
//
// The anchor determines the shape of `_links.target`: branch-anchored at
// HEAD, commit-anchored under a /commits/{sha}/ prefix. This is how
// following a ref stays at the same temporal anchor — the spec invariant
// expressed in TestRefTemporal_StateAtDefinitionTime.
func BuildRefViews(
	b hal.URLBuilder,
	repo string,
	a hal.Anchor,
	raw []string,
	resolver RefResolver,
) []RefView {
	out := make([]RefView, 0, len(raw))
	for _, r := range raw {
		if isExternalRef(r) {
			out = append(out, RefView{Raw: r, Kind: "url"})
			continue
		}
		if !resolver.Exists(r) {
			out = append(out, RefView{Raw: r, Kind: "broken"})
			continue
		}
		out = append(out, RefView{
			Raw:  r,
			Kind: "fact",
			Links: hal.LinkMap{
				"target": {Href: b.Fact(repo, a, r)},
			},
		})
	}
	return out
}
