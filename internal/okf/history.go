// internal/okf/history.go
package okf

import (
	"fmt"
	"sort"
	"strings"
)

// renderHistory renders a fact's revision history — the section that shows how
// a belief EVOLVED rather than only where it landed. Returns "" when there is
// nothing worth showing.
//
// Contract: callers supply revisions oldest-first; revisions sharing a
// timestamp retain the caller's order. That order is the only thing that
// resolves same-timestamp chronology, since nothing else in a Revision
// carries when it happened.
//
// A single revision renders nothing: the fact's birth is already stated by
// `generated.at`, so a one-line History would be noise rather than signal.
// Revisions that changed nothing we track (see describeDelta) are dropped;
// if fewer than two revisions survive that filtering, nothing renders either
// — a fact whose only changes were untracked did not evolve.
func renderHistory(revs []Revision) string {
	if len(revs) < 2 {
		return ""
	}

	// Stable-sort oldest-first on a copy: deltas compare adjacent revisions,
	// and a commit walk does not yield revisions in chronological order.
	// sort.SliceStable preserves the caller's order for equal Dates, which is
	// how same-second revisions keep their true chronology.
	ordered := make([]Revision, len(revs))
	copy(ordered, revs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Date.Before(ordered[j].Date)
	})

	keep, deltas := MeaningfulRevisions(ordered)
	if len(keep) < 2 {
		return ""
	}
	kept := make([]Revision, len(keep))
	for i, k := range keep {
		kept[i] = ordered[k]
	}

	lines := make([]string, 0, len(kept))
	for i, r := range kept {
		var b strings.Builder
		b.WriteString("- " + r.Date.UTC().Format("2006-01-02"))
		if r.Operation != "" {
			b.WriteString(" · " + r.Operation)
		}
		b.WriteString(" — " + deltas[i])
		lines = append(lines, b.String())
	}

	var out strings.Builder
	out.WriteString("# History\n\n")
	for i := len(lines) - 1; i >= 0; i-- { // newest first
		out.WriteString(lines[i] + "\n")
	}
	return out.String()
}

// MeaningfulRevisions selects the revisions worth reporting and names what each
// one changed. revs MUST be oldest-first; the returned indices point into it,
// and deltas[i] describes revs[keep[i]].
//
// It is the single definition of "this change is worth telling a reader about",
// shared by the per-fact `# History` section and by log.md. Two views disagreeing
// on that would be two different claims about the same commit.
//
// The oldest revision is always kept — it is the creation. Every later one is
// compared against the last RETAINED revision rather than its immediate
// predecessor, so a run of no-op commits collapses to nothing instead of a wall
// of "revised" noise.
func MeaningfulRevisions(revs []Revision) (keep []int, deltas []string) {
	if len(revs) == 0 {
		return nil, nil
	}
	keep = append(keep, 0)
	deltas = append(deltas, "created")
	for i := 1; i < len(revs); i++ {
		delta := describeDelta(revs[keep[len(keep)-1]], revs[i])
		if delta == "" {
			continue
		}
		keep = append(keep, i)
		deltas = append(deltas, delta)
	}
	return keep, deltas
}

// describeDelta names what changed between two revisions, in a fixed order so
// output is stable. Confidence is named numerically because it is the clearest
// evidence signal knomit carries; the rest are summarized. Returns "" when
// nothing worth reporting changed, which is how MeaningfulRevisions drops a
// revision entirely rather than emit a dangling "revised" line.
//
// A change to the REF COUNT alone does not count. It is the one field whose
// delta cannot say what actually happened — RefCount is an int, so "refs
// updated" names neither which ref nor why — and on real corpora it was half of
// everything both views emitted. It still rides along as detail on a change that
// earned its line some other way; it just never earns one by itself.
func describeDelta(prev, cur Revision) string {
	var parts []string
	if prev.Confidence != cur.Confidence {
		parts = append(parts, fmt.Sprintf("confidence %g → %g", prev.Confidence, cur.Confidence))
	}
	if prev.Title != cur.Title {
		parts = append(parts, "title changed")
	}
	if prev.BodyDigest != cur.BodyDigest {
		parts = append(parts, "body revised")
	}
	reportable := len(parts) > 0
	if prev.RefCount != cur.RefCount {
		parts = append(parts, "refs updated")
	}
	if !reportable {
		return ""
	}
	return strings.Join(parts, ", ")
}
