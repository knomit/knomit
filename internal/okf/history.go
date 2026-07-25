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
// A single revision renders nothing: the fact's birth is already stated by
// `generated.at`, so a one-line History would be noise rather than signal.
func renderHistory(revs []Revision) string {
	if len(revs) < 2 {
		return ""
	}

	// Sort oldest-first on a copy: deltas compare adjacent revisions, and a
	// commit walk does not yield revisions in chronological order. Ties break
	// on operation then digest so the result is deterministic.
	ordered := make([]Revision, len(revs))
	copy(ordered, revs)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		return a.BodyDigest < b.BodyDigest
	})

	lines := make([]string, 0, len(ordered))
	for i, r := range ordered {
		delta := "created"
		if i > 0 {
			delta = describeDelta(ordered[i-1], r)
		}
		var b strings.Builder
		b.WriteString("- " + r.Date.UTC().Format("2006-01-02"))
		if r.Operation != "" {
			b.WriteString(" · " + r.Operation)
		}
		b.WriteString(" — " + delta)
		lines = append(lines, b.String())
	}

	var out strings.Builder
	out.WriteString("# History\n\n")
	for i := len(lines) - 1; i >= 0; i-- { // newest first
		out.WriteString(lines[i] + "\n")
	}
	return out.String()
}

// describeDelta names what changed between two consecutive revisions, in a
// fixed order so output is stable. Confidence is named numerically because it
// is the clearest evidence signal knomit carries; the rest are summarized.
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
	if len(parts) == 0 {
		// A commit touched the file without changing anything we track
		// (e.g. a field we do not surface). Say so rather than emit a
		// dangling dash.
		return "revised"
	}
	return strings.Join(parts, ", ")
}
