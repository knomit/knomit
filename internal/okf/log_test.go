// internal/okf/log_test.go
package okf

import (
	"strings"
	"testing"
	"time"
)

func TestRenderLog_GroupedNewestFirst(t *testing.T) {
	d1 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	entries := []LogEntry{
		{Date: d1, Kind: "Creation", Title: "Alpha", Path: "kb/a/x/1.md"},
		{Date: d2, Kind: "Update", Title: "Beta", Path: "kb/b/y/2.md", Delta: "confidence 0.9 → 0.85"},
		{Date: d2, Kind: "Creation", Title: "Gamma", Path: "kb/c/z/3.md"},
	}
	got := string(RenderLog(entries))
	want := "# Log\n\n" +
		"## 2026-07-22\n\n" +
		"- **Creation** 1 fact added\n" +
		"- **Update** Beta — confidence 0.9 → 0.85\n\n" +
		"## 2026-07-20\n\n" +
		"- **Creation** 1 fact added\n"
	if got != want {
		t.Fatalf("RenderLog mismatch:\n got:\n%q\nwant:\n%q", got, want)
	}
}

// Creations collapse to a count. Naming all of them is what made log.md 172 KB
// on a real corpus — 1456 of 1778 rows — burying the revisions a reader came
// for. The count must survive, because "the base grew by 47 facts" is the part
// a changelog cannot drop.
func TestRenderLog_CreationsCollapseToACountThatStillReportsGrowth(t *testing.T) {
	day := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	var entries []LogEntry
	for i := 0; i < 47; i++ {
		entries = append(entries, LogEntry{Date: day, Kind: "Creation", Title: "Fact", Path: "kb/a/x/f.md"})
	}
	got := string(RenderLog(entries))
	if !strings.Contains(got, "- **Creation** 47 facts added\n") {
		t.Errorf("growth not reported as a count:\n%s", got)
	}
	if n := strings.Count(got, "- **Creation**"); n != 1 {
		t.Errorf("got %d Creation rows, want 1 collapsed row:\n%s", n, got)
	}
}

// An Update that changed nothing this mapper tracks asserts that something
// happened while being unable to say what. It is dropped, and a day left with
// nothing to report disappears with it.
func TestRenderLog_UpdatesWithoutAReportableDeltaAreDropped(t *testing.T) {
	d1 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	got := string(RenderLog([]LogEntry{
		{Date: d1, Kind: "Creation", Title: "Alpha", Path: "kb/a/x/1.md"},
		{Date: d2, Kind: "Update", Title: "Beta", Path: "kb/b/y/2.md"}, // no Delta
	}))
	if strings.Contains(got, "Beta") {
		t.Errorf("an update with no reportable delta must not be listed:\n%s", got)
	}
	if strings.Contains(got, "2026-07-22") {
		t.Errorf("a day with nothing left to report must not get a heading:\n%s", got)
	}
	if !strings.Contains(got, "2026-07-20") {
		t.Errorf("the day that does have something must survive:\n%s", got)
	}
}

// The month index is links, not headings: §9 requires a flat list whose date
// headings are ISO YYYY-MM-DD, so grouping by month the way views/ does would
// break conformance.
func TestRenderLog_MonthJumpBarIsLinksAndOnlyWhenItHelps(t *testing.T) {
	jun := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)

	multi := string(RenderLog([]LogEntry{
		{Date: jul, Kind: "Creation", Title: "A", Path: "kb/a/x/1.md"},
		{Date: jun, Kind: "Creation", Title: "B", Path: "kb/b/y/2.md"},
	}))
	if !strings.Contains(multi, "**Months:** [2026-07](#2026-07-22) (1) · [2026-06](#2026-06-30) (1)\n") {
		t.Errorf("month jump bar missing or malformed:\n%s", multi)
	}
	for _, h := range []string{"## 2026-07\n", "### "} {
		if strings.Contains(multi, h) {
			t.Errorf("§9 requires flat YYYY-MM-DD headings; found %q:\n%s", h, multi)
		}
	}

	single := string(RenderLog([]LogEntry{
		{Date: jul, Kind: "Creation", Title: "A", Path: "kb/a/x/1.md"},
	}))
	if strings.Contains(single, "**Months:**") {
		t.Errorf("a one-month log gains nothing from an index of one:\n%s", single)
	}
}

// No entries means no document, matching every other view in this package.
func TestRenderLog_EmptyProducesNoFile(t *testing.T) {
	if got := RenderLog(nil); got != nil {
		t.Fatalf("an empty log must produce no file, got %q", got)
	}
	// Only-unreportable updates is the same case: nothing survives the filter.
	got := RenderLog([]LogEntry{
		{Date: time.Now().UTC(), Kind: "Update", Title: "Beta", Path: "kb/b/y/2.md"},
	})
	if got != nil {
		t.Fatalf("a log whose every entry is filtered out must produce no file, got %q", got)
	}
}

// Every event lands in exactly one log. A revision or a withdrawal belongs to
// the folder holding the fact it happened to; when that folder no longer exists
// — its last fact retired — the event falls to the root, which is the only
// scope that still contains it.
func TestPartitionLog_EveryEventLandsInExactlyOnePlace(t *testing.T) {
	d := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	live := map[string]bool{"kb/a/b": true}
	entries := []LogEntry{
		{Date: d, Kind: "Creation", Title: "New", Path: "kb/a/b/1.md"},
		{Date: d, Kind: "Update", Title: "Moved", Path: "kb/a/b/2.md", Delta: "confidence 0.8 → 0.9"},
		{Date: d, Kind: "Deprecation", Title: "Gone", Path: "kb/a/b/3.md", Delta: "retracted"},
		{Date: d, Kind: "Update", Title: "Homeless", Path: "kb/x/y/4.md", Delta: "body revised"},
		{Date: d, Kind: "Deprecation", Title: "Vanished", Path: "kb/x/y/5.md", Delta: "retracted"},
	}
	root, byDir := partitionLog(entries, live)

	if len(byDir) != 1 {
		t.Fatalf("want one folder log, got %d: %v", len(byDir), byDir)
	}
	gotDir := titlesOf(byDir["kb/a/b"])
	if want := []string{"Moved", "Gone"}; !equalStrings(gotDir, want) {
		t.Errorf("kb/a/b log: got %v want %v", gotDir, want)
	}
	// The creation goes to the root even though its folder is live: creations are
	// 93% of all events, and partitioning them empties the root of everything but
	// the deleted corner of the base.
	if want := []string{"New", "Homeless", "Vanished"}; !equalStrings(titlesOf(root), want) {
		t.Errorf("root log: got %v want %v", titlesOf(root), want)
	}
	if len(root)+len(byDir["kb/a/b"]) != len(entries) {
		t.Errorf("partition lost or duplicated events: %d + %d != %d",
			len(root), len(byDir["kb/a/b"]), len(entries))
	}
}

// §9 names **Deprecation** as one of its three conventional labels, and the log
// emitted none — 247 withdrawals on a real corpus lived only in views/retired.md.
func TestRetirementLogEntries_RecordHowAFactLeft(t *testing.T) {
	d := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	got := retirementLogEntries([]Retirement{
		{Date: d, Title: "Dropped", Path: "kb/a/1.md", Kind: RetiredRetracted},
		{Date: d, Title: "Replaced", Path: "kb/a/2.md", Kind: RetiredSuperseded,
			SuccessorPath: "kb/a/3.md"},
		{Date: d, Title: "Absorbed", Path: "kb/a/4.md", Kind: RetiredSuperseded},
	}, RenderOpts{ResolveFact: func(p string) (FactRef, bool) {
		if p == "kb/a/3.md" {
			return FactRef{Path: "kb/a/better.md", Title: "The better statement"}, true
		}
		return FactRef{}, false
	}})

	want := []string{"retracted", "superseded by The better statement", "superseded"}
	for i, e := range got {
		if e.Kind != "Deprecation" {
			t.Errorf("entry %d: kind %q, want Deprecation", i, e.Kind)
		}
		if e.Delta != want[i] {
			t.Errorf("entry %d: delta %q, want %q", i, e.Delta, want[i])
		}
	}
	// A successor is NAMED, never linked: the same row renders into the root log
	// and into a folder log at a different depth, so a relative link correct in
	// one would be broken in the other.
	for _, e := range got {
		if strings.Contains(e.Delta, "](") {
			t.Errorf("a log row must not carry a relative link: %q", e.Delta)
		}
	}
}

// Within a day the labels read as the change does: what arrived, what moved,
// what left. Alphabetical would file a withdrawal between the two.
func TestRenderLog_KindOrderWithinADay(t *testing.T) {
	d := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	got := string(RenderLog([]LogEntry{
		{Date: d, Kind: "Deprecation", Title: "Left", Path: "kb/a/3.md", Delta: "retracted"},
		{Date: d, Kind: "Update", Title: "Moved", Path: "kb/a/2.md", Delta: "body revised"},
		{Date: d, Kind: "Creation", Title: "Arrived", Path: "kb/a/1.md"},
	}))
	want := "# Log\n\n## 2026-07-22\n\n" +
		"- **Creation** 1 fact added\n" +
		"- **Update** Moved — body revised\n" +
		"- **Deprecation** Left — retracted\n"
	if got != want {
		t.Fatalf("order mismatch:\n got:\n%q\nwant:\n%q", got, want)
	}
}

func titlesOf(es []LogEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Title)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
