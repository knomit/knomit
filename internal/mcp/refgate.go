package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// checkLocalRefsResolve rejects a write in which any fact cites a fact in THIS
// repo that will not exist once the call lands.
//
// Forbidding the state is what lets the rest of the system stay simple: because
// a broken local ref cannot be written, nothing downstream has to detect,
// store, or report one — no unresolved-ref table, no extra knomit_explain
// bucket, no UI error state.
//
// A ref is satisfied when its path is either
//   - in this batch — BatchWriteFacts commits the whole call as ONE commit, so
//     facts written together may cite each other in any order, including
//     circularly — or
//   - present on the write branch AND not being retracted by this call.
//
// Only fact.RefLocalFact is gated. A foreign kb:// ref may name an unmounted
// repo; a src:// ref names source objects knomit never holds; an external URL
// is opaque. None is checkable here, so none is gated.
//
// Existence is tested against the branch head — the commit this write lands on
// — which is commit-time resolution for the fact being written, not a HEAD
// judgement imposed on anyone else's refs. Refs in previously-written facts are
// never re-evaluated: they resolve at their own commit, forever.
//
// batch maps each on-disk path being written to that fact's refs. Callers pass
// the authoritative on-disk path (which preserves the configured ontology root
// verbatim, possibly uppercase); comparison is case-folded here because
// ClassifyRef lowercases fact paths and storage is lowercase-canonical.
func checkLocalRefsResolve(
	ctx context.Context,
	fi store.FactIndex,
	branch, localRepoID string,
	batch map[string][]string,
	deletes []string,
) error {
	beingDeleted := make(map[string]bool, len(deletes))
	for _, d := range deletes {
		beingDeleted[strings.ToLower(d)] = true
	}

	// Paths this call will create. A path also being deleted does not count —
	// the delete is what a reader would observe.
	inBatch := make(map[string]bool, len(batch))
	for p := range batch {
		if lp := strings.ToLower(p); !beingDeleted[lp] {
			inBatch[lp] = true
		}
	}

	type problem struct{ from, ref string }
	var problems []problem
	checked := make(map[string]bool) // memoize FactExists per path

	// Deterministic order so the error text is stable across runs.
	sources := make([]string, 0, len(batch))
	for p := range batch {
		sources = append(sources, p)
	}
	sort.Strings(sources)

	for _, from := range sources {
		for _, raw := range batch[from] {
			r := fact.ClassifyRef(raw, localRepoID)
			if r.Kind != fact.RefLocalFact || inBatch[r.Path] {
				continue
			}
			if beingDeleted[r.Path] {
				problems = append(problems, problem{from, raw})
				continue
			}
			ok, seen := checked[r.Path]
			if !seen {
				var err error
				if ok, err = fi.FactExists(ctx, branch, r.Path); err != nil {
					return fmt.Errorf("ref gate: FactExists(%s): %w", r.Path, err)
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
		"it is a typo.\nReferences to other repos (kb://<other-id>/…), to source " +
		"(src://…), and to URLs are not checked and never rejected.")
	return fmt.Errorf("%s", b.String())
}
