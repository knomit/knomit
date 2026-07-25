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

	// Keep the oldest revision unconditionally (it is the "created" line).
	// For each later revision, compute its delta against the last RETAINED
	// revision — not simply its predecessor — and drop it when nothing we
	// track changed, so a run of no-op commits collapses to nothing rather
	// than a wall of "revised" noise.
	kept := ordered[:1]
	deltas := []string{"created"}
	for i := 1; i < len(ordered); i++ {
		last := kept[len(kept)-1]
		delta := describeDelta(last, ordered[i])
		if delta == "" {
			continue
		}
		kept = append(kept, ordered[i])
		deltas = append(deltas, delta)
	}

	if len(kept) < 2 {
		return ""
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

// describeDelta names what changed between two revisions, in a fixed order so
// output is stable. Confidence is named numerically because it is the
// clearest evidence signal knomit carries; the rest are summarized. Returns
// "" when nothing we track changed — the caller (renderHistory) uses that to
// drop the revision entirely rather than emit a dangling "revised" line.
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
	if prev.RefCount != cur.RefCount {
		parts = append(parts, "refs updated")
	}
	return strings.Join(parts, ", ")
}
