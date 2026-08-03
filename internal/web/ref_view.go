package web

import (
	"knomit/internal/fact"
	"knomit/internal/web/hal"
)

// RefView is the on-wire shape for a single entry in a fact's refs array.
//
// Kind is one of:
//   - "fact"        a fact in THIS repo, present at the caller's anchor. The
//     only kind carrying _links.target.
//   - "broken"      a fact in this repo, absent at the caller's anchor.
//   - "foreign"     a fact in ANOTHER knomit repo (kb://<other-id>/…). NOT
//     broken — merely not ours to resolve. Linking it is the
//     cross-mount hop gap named in
//     kb/gotchas/lens/browsing-ui-accepted-gaps/595c0c7b.md.
//   - "source_code" a src:// citation. Terminal: knomit's object database
//     holds fact blobs only, never source, so this never
//     resolves here. Named "source_code" and not "source"
//     because facts already carry a `sources` field.
//   - "url"         http(s)://, file://, or any other scheme.
//
// Path is the repo-relative fact path, set for the "fact" and "broken" kinds
// only — the two the client acts on. It exists so the client never has to
// recover a path from Raw: a canonical kb://<own-id>/<path> ref and its bare
// equivalent name the same fact, and deciding that is ClassifyRef's job, not a
// regex in the browser. Omitted for foreign, source_code and url, where no path
// in THIS repo is meaningful.
//
// Display is the compact LABEL a UI shows in place of Raw — currently set for
// "source_code" only, where a 12-hex repo id and two 40-hex hashes make the raw
// citation unreadable at a glance. It is computed here, from the same
// fact.ClassifyRef parse that produced Kind, because taking a src:// ref apart
// is ref parsing and this package's client must never do that (see
// kb/invariants/ui/factbody/ref-scheme-branching). Empty means "no display form
// — render Raw", which is the correct rendering for every other kind. A client
// showing Display must keep Raw reachable, as a title/tooltip at minimum: the
// stored citation is what a reader copies.
type RefView struct {
	Raw     string      `json:"raw"`
	Kind    string      `json:"kind"`
	Path    string      `json:"path,omitempty"`
	Display string      `json:"display,omitempty"`
	Links   hal.LinkMap `json:"_links,omitempty"`
}

// RepoNamer answers "which mounted repo is this 12-hex id?", returning "" when
// none is. Production supplies repos.Manager.NameByID; a nil RepoNamer is legal
// and means no id can be named, which leaves ids in place rather than inventing
// one. It is a func rather than the Manager itself so this file keeps its narrow
// dependency and tests can name ids without building a Manager.
type RepoNamer func(id12 string) string

// nameOf applies a possibly-nil RepoNamer. A nil namer and an unknown id are
// the same answer — "" — so Display keeps the raw id.
func nameOf(namer RepoNamer, id12 string) string {
	if namer == nil {
		return ""
	}
	return namer(id12)
}

// RefResolver is used by BuildRefViews to decide whether a fact-path ref is
// navigable at the current anchor. Tests provide stub resolvers; production
// uses FactQuery.FactExistsAt via readerRefResolver, which has already been
// handed the caller's anchor.
type RefResolver interface {
	// Exists returns true when a fact exists at the given repo-relative path,
	// visible at the caller's anchor.
	Exists(path string) bool
}

// BuildRefViews transforms a fact's raw refs into structured RefView entries.
//
// KIND comes from fact.ClassifyRef — the single ref-classification authority —
// so this surface cannot drift from the edge builder, replay, knomit_explain,
// or the web client. The local heuristic it replaced ("external if http(s) OR
// not .md") mislabelled two whole categories: a cross-repo kb://<other>/z.md
// ref ends in .md, so it fell through to the resolver, which looked for a local
// fact literally named "kb://<other>/z.md", found nothing, and reported the ref
// as BROKEN; and a src:// citation does not end in .md, so it was reported as a
// "url" the client then rendered as a clickable anchor for a scheme no browser
// can open.
//
// RESOLUTION is unchanged and stays commit-anchored: the resolver already holds
// the caller's anchor, so Exists answers "did this fact exist at the version
// being viewed", not "does it exist now". That is why the fact/broken split
// lives here and not in ClassifyRef, which is pure and has no anchor.
//
// The anchor also shapes _links.target: branch-anchored at HEAD,
// commit-anchored under a /commits/{sha}/ prefix, so following a ref stays at
// the same temporal anchor.
//
// localRepoID is the viewing repo's 12-hex id, used only to tell a
// self-referential kb:// ref from a foreign one. Empty means every kb:// ref
// reads as foreign — under-reporting rather than inventing local links.
//
// namer resolves a src:// ref's repo id to a mounted repo's name for the
// Display label; nil (or an id it does not know) leaves the id in place.
func BuildRefViews(
	b hal.URLBuilder,
	repo string,
	a hal.Anchor,
	raw []string,
	resolver RefResolver,
	localRepoID string,
	namer RepoNamer,
) []RefView {
	out := make([]RefView, 0, len(raw))
	for _, r := range raw {
		c := fact.ClassifyRef(r, localRepoID)

		switch c.Kind {
		case fact.RefLocalFact:
			// Ask the resolver for the REPO-RELATIVE path. Handing it the raw
			// kb://<id>/… string is what made canonical-form refs unresolvable.
			if !resolver.Exists(c.Path) {
				out = append(out, RefView{Raw: r, Kind: "broken", Path: c.Path})
				continue
			}
			out = append(out, RefView{
				Raw:   r,
				Kind:  "fact",
				Path:  c.Path,
				Links: hal.LinkMap{"target": {Href: b.Fact(repo, a, c.Path)}},
			})

		case fact.RefForeignFact:
			out = append(out, RefView{Raw: r, Kind: "foreign", Display: c.Display(nameOf(namer, c.RepoID))})

		case fact.RefSourceCode:
			out = append(out, RefView{Raw: r, Kind: "source_code", Display: c.Display(nameOf(namer, c.RepoID))})

		default:
			// RefExternalURL, and RefMalformed. Malformed maps to "url" rather
			// than gaining a sixth kind: ParseFact and SerializeFact both
			// reject malformed refs, so only a hand-edited file reaches here.
			out = append(out, RefView{Raw: r, Kind: "url"})
		}
	}
	return out
}
