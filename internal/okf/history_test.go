// internal/okf/history_test.go
package okf

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRenderHistory_StableSortAtScale pins sort STABILITY, not just order:
// same-timestamp revisions must keep the caller's order, because nothing else
// in a Revision says which came first.
//
// Detecting a sort.SliceStable → sort.Slice regression needs the right fixture.
// Below n≈13 sort.Slice is insertion sort and accidentally stable; ABOVE that
// it is pdqsort, which still leaves an already-ordered input untouched because
// its partial-insertion-sort pass finds zero inversions and bails. So neither a
// small fixture NOR a large all-equal one can tell the two apart.
//
// What bites is the shape the sort exists for: a commit walk yields revisions
// newest-first, so dates arrive out of chronological order, with same-second
// revisions tied inside each group. There pdqsort genuinely reorders ties, and
// a swapped pair renders a BACKWARDS confidence delta — the exact bug.
func TestRenderHistory_StableSortAtScale(t *testing.T) {
	const groups, perGroup = 10, 2
	const n = groups * perGroup
	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	// conf is strictly increasing in true chronological order, so any inversion
	// in the rendered output is visible as a backwards delta.
	conf := func(seq int) float64 { return 0.5 + float64(seq)/100 }

	// Supplied newest-GROUP-first; within a group, true chronological order.
	revs := make([]Revision, 0, n)
	for g := groups - 1; g >= 0; g-- {
		for k := 0; k < perGroup; k++ {
			seq := g*perGroup + k
			revs = append(revs, Revision{
				Date:       base.Add(time.Duration(g) * time.Minute),
				Operation:  "update",
				Confidence: conf(seq),
				Title:      "T",
				BodyDigest: "d" + strconv.Itoa(seq),
			})
		}
	}

	got := renderHistory(revs)
	// The trailing comma anchors the match: without it "confidence 0.51 → 0.5"
	// is a prefix of the legitimate "confidence 0.51 → 0.52". Every revision
	// here also changes its body digest, so ", body revised" always follows.
	for seq := 1; seq < n; seq++ {
		back := fmt.Sprintf("confidence %g → %g,", conf(seq), conf(seq-1))
		if strings.Contains(got, back) {
			t.Fatalf("backwards delta %q — the sort is not stable:\n%s", back, got)
		}
	}
}

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

// A revision whose ONLY change is the ref count earns no line: RefCount is an
// int, so "refs updated" names neither which ref nor why, and on a real corpus
// it was half of everything both views emitted. It still rides along as detail
// on a revision that earned its line some other way — note the 07-24 row, whose
// ref count also moved (0 → 2 across the dropped revision).
func TestRenderHistory_RefCountAloneEarnsNoLine(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.72, "T", "d1", 0),   // created
		rev(18, "review", 0.72, "T", "d1", 2),  // refs only — dropped
		rev(24, "distill", 0.90, "T", "d2", 2), // confidence + body
	}
	got := renderHistory(revs)

	want := "# History\n\n" +
		"- 2026-07-24 · distill — confidence 0.72 → 0.9, body revised, refs updated\n" +
		"- 2026-07-11 · learn — created\n"
	if got != want {
		t.Fatalf("history mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// And when the ref count is the only thing that ever moved, the section itself
// does not render: a fact whose refs were retouched did not evolve.
func TestRenderHistory_RefOnlyHistoryRendersNothing(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.72, "T", "d1", 0),
		rev(18, "review", 0.72, "T", "d1", 2),
		rev(24, "review", 0.72, "T", "d1", 3),
	}
	if got := renderHistory(revs); got != "" {
		t.Fatalf("ref-count churn is not evolution, got:\n%s", got)
	}
}

func TestRenderHistory_TitleChangeAndNoOpDropped(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.9, "Old title", "d1", 1),
		rev(12, "update", 0.9, "New title", "d1", 1), // title only
		rev(13, "", 0.9, "New title", "d1", 1),       // nothing detectable, no operation: dropped
	}
	got := renderHistory(revs)
	if !strings.Contains(got, "- 2026-07-12 · update — title changed\n") {
		t.Errorf("title-only delta wrong:\n%s", got)
	}
	// The no-op revision (day 13) changed nothing we track relative to the
	// last retained revision (day 12), so it must not appear at all — not as
	// a "revised" line, not under any other label. Match the em-dash line form
	// rather than the bare word: "body revised" is a legitimate delta part, so
	// a bare substring check would false-positive on real output.
	if strings.Contains(got, "— revised") {
		t.Errorf("no-op revision should be dropped, not rendered as \"revised\":\n%s", got)
	}
	if strings.Contains(got, "2026-07-13") {
		t.Errorf("no-op revision's date should not appear:\n%s", got)
	}
	if n := strings.Count(got, "\n- "); n != 2 {
		t.Errorf("expected exactly 2 lines after dropping the no-op, got %d:\n%s", n, got)
	}
}

// Same-second revisions carry no chronology of their own: the ONLY thing that
// orders them is the sequence the caller supplied them in. The old code
// broke ties on Operation then BodyDigest, which has nothing to do with when
// a revision happened and could silently reverse the delta.
func TestRenderHistory_SameSecondOrderPreserved(t *testing.T) {
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	older := Revision{Date: t0, Operation: "zzz-old", Confidence: 0.5, Title: "T", BodyDigest: "zzz", RefCount: 0}
	newer := Revision{Date: t0, Operation: "aaa-new", Confidence: 0.9, Title: "T", BodyDigest: "aaa", RefCount: 0}

	// Supplied oldest-first, per the mapper's contract. The old tie-break
	// (Operation "aaa-new" < "zzz-old") would have sorted `newer` before
	// `older`, reversing the delta to 0.9 → 0.5.
	got := renderHistory([]Revision{older, newer})

	if !strings.Contains(got, "confidence 0.5 → 0.9") {
		t.Errorf("expected forward delta 0.5 → 0.9, got:\n%s", got)
	}
	if strings.Contains(got, "confidence 0.9 → 0.5") {
		t.Errorf("delta rendered backwards — same-second tie-break reordered revisions:\n%s", got)
	}
}

// A no-op revision must not break the delta chain: the next real change is
// compared against the last RETAINED revision, so it correctly spans the
// skipped no-op instead of comparing against it.
func TestRenderHistory_NoOpSkippedInDeltaChain(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.5, "T", "d1", 0),   // created
		rev(12, "review", 0.5, "T", "d1", 0),  // no-op: identical to creation
		rev(13, "distill", 0.9, "T", "d1", 0), // real change: confidence only
	}
	got := renderHistory(revs)

	if n := strings.Count(got, "\n- "); n != 2 {
		t.Fatalf("expected exactly 2 lines (creation + real change), got %d:\n%s", n, got)
	}
	// Match the em-dash line form, not the bare word: "body revised" is a
	// legitimate delta part that a bare substring check would trip over.
	if strings.Contains(got, "— revised") {
		t.Errorf("no line should read \"revised\":\n%s", got)
	}
	// The real change's delta must span the skipped no-op, comparing against
	// the creation (0.5) rather than against the no-op revision it followed.
	if !strings.Contains(got, "- 2026-07-13 · distill — confidence 0.5 → 0.9\n") {
		t.Errorf("delta should span the skipped no-op, comparing against the creation:\n%s", got)
	}
}

// If filtering leaves fewer than two revisions, the fact did not evolve in
// any way the bundle tracks, so nothing renders — same rule as a fact with
// only one revision to begin with.
func TestRenderHistory_SuppressedAfterFilteringNoOps(t *testing.T) {
	revs := []Revision{
		rev(11, "learn", 0.5, "T", "d1", 0),  // created
		rev(12, "review", 0.5, "T", "d1", 0), // no-op
	}
	if got := renderHistory(revs); got != "" {
		t.Errorf("only one revision survives filtering, so nothing should render, got:\n%s", got)
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
