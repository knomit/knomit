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
