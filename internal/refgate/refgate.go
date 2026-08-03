// Package refgate is the write-time authority over a fact's references.
//
// It exists so the rule "a stored local fact ref resolves" is a property of
// the CORPUS rather than of one entry point. Every write path routes through
// the same Gate — knomit_learn, knomit_update, the synthesize pipelines, and
// the REST create/update handlers — so a ref cannot be laundered past the check
// by choosing a different API. That is also why the logic lives here and not in
// internal/mcp: a copy in internal/web would be the ninth copy of a rule this
// codebase has just finished collapsing into one (see fact.ClassifyRef).
//
// # The temporal contract
//
// kb/principles/philosophy/historical-not-current: "Every ref, edge, and
// provenance link resolves at the commit point in time of the referrer — never
// at HEAD." Two consequences shape this package, and violating either one
// silently rewrites history:
//
//  1. A ref is checked ONCE, against the commit the write lands on — the
//     referrer's own commit. Refs a fact ALREADY carried are never re-checked:
//     they resolved at their commit and that is a fact about the past, not a
//     claim about now. Callers pass those as `prior`. Without this, editing a
//     fact's title re-litigates citations written months earlier against
//     today's corpus, and a retraction anywhere in history makes every fact
//     that ever cited it uneditable.
//
//  2. The gate asks EXACTLY the question the reader asks — FactExistsAt, which
//     walks back through retractions. A retracted fact still has a navigable
//     last-valid blob, so a ref to it renders as a live `fact` link in the UI
//     and follows correctly in knomit_explain. A gate that asked the narrower
//     "is it live right now" would reject writes whose refs the rest of the
//     system resolves perfectly well — the gate would be enforcing a stricter
//     reality than the one the graph actually implements.
//
// Kind classification stays in internal/fact, which is pure. This package adds
// the two things that need corpus access and repo identity: resolution and
// canonicalization.
package refgate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// ResolveFunc reports whether a ref target resolves at the commit this write
// lands on, walking back through retractions the way every reader does.
//
// This MUST be the reader's predicate (store.FactQuery.FactExistsAt with an
// empty commit), not a live-only existence check — see consequence 2 above.
//
// Injected rather than taking a store.FactQuery directly because the callers
// reach the store differently: internal/mcp and internal/synthesize hold a
// FactQuery, while internal/web reaches it through RepoInstance.WithRead per
// request. A function is the only shape all of them satisfy without one growing
// another's plumbing.
type ResolveFunc func(ctx context.Context, path string) (bool, error)

// FromFactQuery is the ResolveFunc for a caller that already holds a FactQuery.
// The empty commit means "at the branch head", which is the commit the write is
// about to become the parent of — the referrer's own commit.
func FromFactQuery(fq store.FactQuery, branch string) ResolveFunc {
	return func(ctx context.Context, path string) (bool, error) {
		return fq.FactExistsAt(ctx, branch, path, "")
	}
}

// Gate checks and canonicalizes the refs of a write before it lands.
//
// localRepoID is the writing repo's 12-hex id. Empty means identity is
// unresolvable: every kb:// ref then reads as foreign, so nothing is gated and
// nothing is qualified. That under-enforces rather than rejecting good writes
// or stamping a wrong id into stored bytes, which is the safe direction.
type Gate struct {
	localRepoID string
	resolves    ResolveFunc
}

// New builds a Gate.
func New(localRepoID string, resolves ResolveFunc) Gate {
	return Gate{localRepoID: localRepoID, resolves: resolves}
}

// LocalRepoID exposes the id the gate was built with, for callers that must
// classify refs consistently with it.
func (g Gate) LocalRepoID() string { return g.localRepoID }

// CheckBatch rejects a write in which any NEWLY ADDED ref cites a fact in THIS
// repo that will not resolve once the call lands.
//
// Forbidding the state is what lets the rest of the system stay simple: because
// a broken local ref cannot be introduced, nothing downstream has to detect,
// store, or report one — no unresolved-ref table, no extra knomit_explain
// bucket, no UI error state.
//
// batch maps each on-disk path being written to that fact's complete ref list.
// prior maps the same paths to the refs each fact ALREADY carried; anything in
// prior is skipped, because it resolved at its own commit and this package does
// not re-judge the past. Pass nil for a fact being created.
//
// A newly added ref is satisfied when its path is any of:
//   - in this batch — BatchWriteFacts commits the whole call as ONE commit, so
//     facts written together may cite each other in any order, including
//     circularly;
//   - resolvable per ResolveFunc, which includes a fact retracted earlier and
//     still reachable by walk-back.
//
// There is deliberately no "retracted by this call" parameter. Subsumption —
// an observation settling a hypothesis, a distilled fact replacing its sources
// — cites what it retracts, and that citation is satisfied twice over: the
// target is still live at the pre-write head, and it stays reachable by
// walk-back afterwards. A parameter that can never change an outcome is worse
// than no parameter, because it reads like it is load-bearing.
//
// Only fact.RefLocalFact is gated. A foreign kb:// ref may name an unmounted
// repo; a src:// ref names source objects knomit never holds; an external URL
// is opaque. None is checkable here, so none is gated.
//
// Callers pass the authoritative on-disk path (which preserves the configured
// ontology root verbatim, possibly uppercase); comparison is case-folded here
// because ClassifyRef lowercases fact paths and storage is lowercase-canonical.
func (g Gate) CheckBatch(ctx context.Context, batch, prior map[string][]string) error {
	inBatch := make(map[string]bool, len(batch))
	for p := range batch {
		inBatch[strings.ToLower(p)] = true
	}

	type problem struct{ from, ref string }
	var problems []problem
	checked := make(map[string]bool) // memoize the resolution lookup per path

	// Deterministic order so the error text is stable across runs.
	sources := make([]string, 0, len(batch))
	for p := range batch {
		sources = append(sources, p)
	}
	sort.Strings(sources)

	for _, from := range sources {
		carried := g.pathSet(prior[from])
		for _, raw := range batch[from] {
			r := fact.ClassifyRef(raw, g.localRepoID)
			if r.Kind != fact.RefLocalFact || inBatch[r.Path] || carried[r.Path] {
				continue
			}
			ok, seen := checked[r.Path]
			if !seen {
				if g.resolves == nil {
					return fmt.Errorf("ref gate: no resolver configured, cannot verify %q", r.Path)
				}
				var err error
				if ok, err = g.resolves(ctx, r.Path); err != nil {
					return fmt.Errorf("ref gate: resolve(%s): %w", r.Path, err)
				}
				checked[r.Path] = ok
			}
			if !ok {
				problems = append(problems, problem{from, raw})
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}

	// The reader is an agent that must fix the refs and retry, so name every
	// problem at once, echo each ref exactly as it was sent (so the agent can
	// string-match its own payload), and say what the three fixes are.
	var b strings.Builder
	b.WriteString("unresolvable fact references — nothing was written:\n")
	for _, p := range problems {
		fmt.Fprintf(&b, "  %s cites %s, which does not exist\n", p.from, p.ref)
	}
	b.WriteString("\nEither write the referenced fact in THIS SAME call (all facts in one " +
		"call are committed together, so they may reference each other in any order, " +
		"including circularly), write it first in an earlier call, or fix the path if " +
		"it is a typo.\nOnly refs this write ADDS are checked; refs the fact already " +
		"carried resolve at their own commit and are never re-judged.\nReferences to " +
		"other repos (kb://<other-id>/…), to source (src://…), and to URLs are not " +
		"checked and never rejected.")
	return fmt.Errorf("%s", b.String())
}

// pathSet indexes refs by their classified local path, so a ref carried in one
// form (bare) matches the same ref supplied in the other (canonical). Comparing
// raw strings would call a canonicalized carry-forward "new" and re-check it.
func (g Gate) pathSet(refs []string) map[string]bool {
	if len(refs) == 0 {
		return nil
	}
	set := make(map[string]bool, len(refs))
	for _, raw := range refs {
		if r := fact.ClassifyRef(raw, g.localRepoID); r.Kind == fact.RefLocalFact {
			set[r.Path] = true
		}
	}
	return set
}

// Canonicalize rewrites every ref naming a fact in THIS repo into the canonical
// kb://<own-id>/<path> form, leaving all other kinds untouched. changed reports
// whether anything moved, so a caller can skip re-serializing content it would
// otherwise rewrite byte-for-byte.
//
// This is why an author never needs to know a repo id: bare paths are accepted
// on input and qualified here, on write. A stored ref is then unambiguous
// wherever the fact travels — federated through a lens, exported to a bundle,
// or read by another repo — where a bare path names nothing in particular.
//
// Invisible to every consumer: the edge builder, knomit_explain, the fact API
// and the web client all address a local ref by ClassifyRef's repo-relative
// Path, which is identical for both forms. Only the stored bytes change, and
// only for the version being written — historical versions keep the form they
// were written with, which resolves identically.
//
// A no-op when localRepoID is empty (identity unresolvable) — leaving a bare
// path is strictly better than qualifying it with a wrong or empty id.
func (g Gate) Canonicalize(refs []string) (out []string, changed bool) {
	if g.localRepoID == "" || len(refs) == 0 {
		return refs, false
	}
	// Deduped: two refs that differed only in form — "kb/x.md" and
	// "kb://<own-id>/kb/x.md" — are the same edge, and collapse to one string
	// here. Without this, canonicalizing would MANUFACTURE a duplicate that no
	// caller wrote.
	out = make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, raw := range refs {
		canon := raw
		if r := fact.ClassifyRef(raw, g.localRepoID); r.Kind == fact.RefLocalFact {
			canon = fact.QualifyKBPath(g.localRepoID, r.Path)
		}
		if seen[canon] {
			continue
		}
		seen[canon] = true
		out = append(out, canon)
	}
	if len(out) != len(refs) {
		return out, true
	}
	for i := range out {
		if out[i] != refs[i] {
			return out, true
		}
	}
	return out, false
}

// Apply is the single-fact entry point: check, then canonicalize. It is what
// knomit_update, the pipelines and the REST handlers all call, so they share one
// implementation rather than four transcriptions of the same two steps.
//
// prior is the refs this fact already carried; pass nil when creating one.
//
// Canonicalization runs AFTER the check so a rejection still echoes each ref
// exactly as the caller sent it.
func (g Gate) Apply(ctx context.Context, path string, refs, prior []string) (out []string, changed bool, err error) {
	var priorMap map[string][]string
	if len(prior) > 0 {
		priorMap = map[string][]string{path: prior}
	}
	if err := g.CheckBatch(ctx, map[string][]string{path: refs}, priorMap); err != nil {
		return nil, false, err
	}
	out, changed = g.Canonicalize(refs)
	return out, changed, nil
}
