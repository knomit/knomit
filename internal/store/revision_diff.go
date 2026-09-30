package store

import (
	"fmt"

	"github.com/pmezard/go-difflib/difflib"

	"knomit/internal/fact"
)

// RevisionDiff is a revision's delta from the content it was edited from.
// Each field is present only when it changed; a nil RevisionDiff means "no
// tracked change" (e.g. the fact's creation, which has no predecessor).
// Stored as JSON in path_changes.diff and returned by knomit_explain as-is.
type RevisionDiff struct {
	Confidence []float64 `json:"confidence,omitempty"` // [old, new]
	Body       string    `json:"body,omitempty"`       // smaller of unified diff vs "+N/-M"
}

// minUnifiedDiffLen bounds the shortest unified diff difflib can produce from
// below: a hunk header ("@@ -1 +1 @@\n", 12 bytes) plus at least one changed
// line (2 bytes). A "+N/-M" magnitude only exceeds it past 10^5 lines.
// TestBodyDelta_UnifiedNeverShorterThanBound pins it.
const minUnifiedDiffLen = 13

// bodyDelta returns "" if the bodies are identical, else the smaller of a
// unified diff and a compact "+added/-removed" magnitude. The unified diff is
// only built when it could be the smaller one — never, for a magnitude
// shorter than the shortest possible unified diff — because building it costs
// a second full match of both bodies.
func bodyDelta(a, b string) string {
	if a == b {
		return ""
	}
	aLines := difflib.SplitLines(a)
	bLines := difflib.SplitLines(b)
	added, removed := 0, 0
	for _, op := range difflib.NewMatcher(aLines, bLines).GetOpCodes() {
		switch op.Tag {
		case 'r':
			removed += op.I2 - op.I1
			added += op.J2 - op.J1
		case 'd':
			removed += op.I2 - op.I1
		case 'i':
			added += op.J2 - op.J1
		}
	}
	magnitude := fmt.Sprintf("+%d/-%d", added, removed)
	if len(magnitude) <= minUnifiedDiffLen {
		return magnitude
	}
	unified, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: aLines, B: bLines, Context: 1})
	if unified != "" && len(unified) < len(magnitude) {
		return unified
	}
	return magnitude
}

// revisionDelta computes the diff from prev → cur. nil when there is no
// predecessor or nothing tracked changed.
func revisionDelta(prev *fact.Fact, cur fact.Fact) *RevisionDiff {
	if prev == nil {
		return nil
	}
	d := &RevisionDiff{}
	changed := false
	if prev.Confidence != cur.Confidence {
		d.Confidence = []float64{prev.Confidence, cur.Confidence}
		changed = true
	}
	if body := bodyDelta(prev.Body, cur.Body); body != "" {
		d.Body = body
		changed = true
	}
	if !changed {
		return nil
	}
	return d
}
