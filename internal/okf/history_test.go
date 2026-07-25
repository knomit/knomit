// internal/okf/history_test.go
package okf

import (
	"strings"
	"testing"
	"time"
)

func rev(day int, op string, conf float64, title, digest string, refs int) Revision {
	return Revision{
		Date:       time.Date(2026, 7, day, 9, 0, 0, 0, time.UTC),
		Operation:  op,
		Confidence: conf,
		Title:      title,
		BodyDigest: digest,
		RefCount:   refs,
	}
}

func TestRenderHistory_SuppressedBelowTwoRevisions(t *testing.T) {
	if got := renderHistory(nil); got != "" {
		t.Errorf("no revisions should render nothing, got:\n%s", got)
	}
	one := []Revision{rev(11, "learn", 0.7, "T", "d1", 0)}
	if got := renderHistory(one); got != "" {
		t.Errorf("a single revision should render nothing (generated.at already states it), got:\n%s", got)
	}
}

func TestRenderHistory_DeltasAndOrder(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.72, "T", "d1", 0),   // created
		rev(18, "review", 0.72, "T", "d1", 2),  // refs only
		rev(24, "distill", 0.90, "T", "d2", 2), // confidence + body
	}
	got := renderHistory(revs)

	want := "# History\n\n" +
		"- 2026-07-24 · distill — confidence 0.72 → 0.9, body revised\n" +
		"- 2026-07-18 · review — refs updated\n" +
		"- 2026-07-11 · learn — created\n"
	if got != want {
		t.Fatalf("history mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderHistory_TitleChangeAndNoOpFallback(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.9, "Old title", "d1", 1),
		rev(12, "update", 0.9, "New title", "d1", 1), // title only
		rev(13, "", 0.9, "New title", "d1", 1),       // nothing detectable, no operation
	}
	got := renderHistory(revs)
	if !strings.Contains(got, "- 2026-07-12 · update — title changed\n") {
		t.Errorf("title-only delta wrong:\n%s", got)
	}
	// No operation ⇒ the line carries no separator, just date and delta.
	if !strings.Contains(got, "- 2026-07-13 — revised\n") {
		t.Errorf("no-op fallback wrong:\n%s", got)
	}
}

// Revisions may arrive in any order (a commit walk is not chronological), so
// rendering must sort them or the deltas compare the wrong pairs.
func TestRenderHistory_SortsUnorderedInput(t *testing.T) {
	revs := []Revision{
		rev(24, "distill", 0.90, "T", "d2", 0),
		rev(11, "learn", 0.72, "T", "d1", 0),
	}
	got := renderHistory(revs)
	// The two revisions differ in confidence AND body digest, so the newest
	// line carries both delta parts in their fixed order.
	want := "# History\n\n" +
		"- 2026-07-24 · distill — confidence 0.72 → 0.9, body revised\n" +
		"- 2026-07-11 · learn — created\n"
	if got != want {
		t.Fatalf("unordered input mishandled:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
