package web

import (
	"encoding/json"
	"strings"

	knomitfact "knomit/internal/fact"
	"knomit/internal/web/hal"
)

// FactView is the on-wire HAL envelope for a single fact. One serializer
// handles both HEAD-anchored and commit-anchored views; the anchor passed
// to BuildFactView determines the link set.
type FactView struct {
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	Body       string    `json:"body,omitempty"` // omitted in collection items (hard rule §3 #8)
	Kind       string    `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type       string    `json:"type,omitempty"`
	Domain     []string  `json:"domain"`
	Entities   []string  `json:"entities"`
	Refs       []RefView `json:"refs"`
	Confidence float64   `json:"confidence"`
	Sources    int       `json:"sources"`
	Origin     string    `json:"origin,omitempty"` // omitted when authored (the default)
	AsOf       AsOf      `json:"as_of"`

	// RefWarnings surfaces refs whose SHAPE is malformed. ParseFact reads such
	// a fact rather than failing — a version legal when committed must stay
	// readable — so this is how a reader learns the citation is unfollowable
	// instead of the fact silently vanishing from the index.
	RefWarnings []string `json:"ref_warnings,omitempty"`

	// Links is public so tests can inspect it. Marshaled as _links.
	Links hal.LinkMap `json:"-"`
}

// AsOf captures the temporal anchor of a fact view.
type AsOf struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	// Date is when the VERSION OF THIS FACT on display was committed —
	// RFC3339, UTC. Not the anchor commit's date: in a HEAD view the anchor is
	// the branch tip, and its date says when the repo last changed, which has
	// nothing to do with the fact being read.
	//
	// omitempty is load-bearing: an unresolvable commit must omit the field so
	// the client renders nothing, never a zero time formatted as 1970.
	Date string `json:"date,omitempty"`
}

// MarshalJSON emits FactView with the _links map under its canonical key.
func (v FactView) MarshalJSON() ([]byte, error) {
	type plain FactView
	wrap := struct {
		plain
		Links hal.LinkMap `json:"_links"`
	}{plain: plain(v), Links: v.Links}
	return json.Marshal(wrap)
}

// BuildFactView serializes a knomitfact.Fact into a HAL FactView for the
// given anchor. The repo name and URLBuilder give it the absolute URLs;
// the resolver is used for each fact-path ref to decide `kind` and
// target-link presence.
//
// headCommit is the resolved HEAD sha of the branch, used for the `snapshot`
// link on HEAD views and for the `as_of.commit` stamp. It is independent of
// the anchor: a HEAD view has `anchor.Commit == ""` AND a non-empty
// headCommit; a commit-anchored view has `anchor.Commit == sha` AND the
// headCommit is typically irrelevant (callers may pass the same sha or
// leave it empty — only `anchor.Commit` is used for as_of in that case).
func BuildFactView(
	b hal.URLBuilder,
	repo string,
	a hal.Anchor,
	headCommit string,
	f knomitfact.Fact,
	resolver RefResolver,
	localRepoID string,
) FactView {
	asOfCommit := a.Commit
	if asOfCommit == "" {
		asOfCommit = headCommit
	}
	// Mirror fact.Fact.MarshalJSON: elide Kind when it equals the default so
	// epistemic facts serialize without the field (omitempty).
	kind := string(f.Kind)
	if f.Kind == knomitfact.DefaultKind {
		kind = ""
	}
	// Mirror fact.Fact.MarshalJSON: elide Origin when it equals the default
	// (authored) so the chip surfaces only the interesting machine origins
	// (distilled, discovered).
	origin := string(f.Origin)
	if f.Origin == knomitfact.DefaultOrigin {
		origin = ""
	}
	v := FactView{
		Path:        f.Path(),
		Title:       f.Title,
		Body:        f.Body,
		Kind:        kind,
		Type:        string(f.Type),
		Domain:      f.Domain,
		Entities:    f.Entities,
		Confidence:  f.Confidence,
		Sources:     f.Sources,
		Origin:      origin,
		AsOf:        AsOf{Branch: a.Branch, Commit: asOfCommit},
		Refs:        BuildRefViews(b, repo, a, f.Refs, resolver, localRepoID),
		RefWarnings: f.RefWarnings,
	}
	v.Links = buildFactLinks(b, repo, a, headCommit, f.Path())
	return v
}

// buildFactLinks constructs the _links map for a fact envelope. The link
// set depends on whether the anchor is HEAD or a specific commit:
//
//	HEAD view:       self, incoming, outgoing, commits, snapshot, parent, branch
//	Commit-anchored: self, incoming, outgoing, commits, live, commit, parent, branch
//
// The `parent` link points at the topic node containing this fact.
// `snapshot` on a HEAD view is the commit-anchored citation pin at the
// current head sha; it's the only way to get a stable URL out of a HEAD
// view. On a commit-anchored view, `self` is already the stable URL so
// `snapshot` is omitted (it would duplicate `self`). `incoming` appears on
// both shapes: commit-anchored incoming returns the version-aware lineage.
func buildFactLinks(
	b hal.URLBuilder,
	repo string,
	a hal.Anchor,
	headCommit string,
	path string,
) hal.LinkMap {
	branchURL := b.Branch(repo, hal.Anchor{Branch: a.Branch})
	links := hal.LinkMap{
		"self":     {Href: b.Fact(repo, a, path)},
		"incoming": {Href: b.FactIncoming(repo, a, path)},
		"outgoing": {Href: b.FactOutgoing(repo, a, path)},
		"commits":  {Href: b.FactCommits(repo, a, path)},
		"parent":   {Href: factParentTopic(b, repo, a, path)},
		"branch":   {Href: branchURL},
	}
	if a.IsHEAD() {
		if headCommit != "" {
			snapshotAnchor := hal.Anchor{Branch: a.Branch, Commit: headCommit}
			links["snapshot"] = hal.Link{Href: b.Fact(repo, snapshotAnchor, path)}
		}
	} else {
		headAnchor := hal.Anchor{Branch: a.Branch}
		links["live"] = hal.Link{Href: b.Fact(repo, headAnchor, path)}
		links["commit"] = hal.Link{Href: branchURL + "/commits/" + a.Commit}
	}
	return links
}

// factParentTopic derives the parent topic URL from a fact path. The fact
// `know/ai/ml/abc12345.md` has parent topic `/topics/ai/ml`. The first
// path segment is the ontology root ("know") and is stripped.
func factParentTopic(b hal.URLBuilder, repo string, a hal.Anchor, path string) string {
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return b.BranchOrCommitPrefix(repo, a) + "/topics"
	}
	// Drop the filename (last segment) and the ontology root (first segment).
	parent := segs[1 : len(segs)-1]
	if len(parent) == 0 {
		return b.BranchOrCommitPrefix(repo, a) + "/topics"
	}
	return b.BranchOrCommitPrefix(repo, a) + "/topics/" + strings.Join(parent, "/")
}
