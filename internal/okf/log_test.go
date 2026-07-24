// internal/okf/log_test.go
package okf

import (
	"testing"
	"time"
)

func TestRenderLog_GroupedNewestFirst(t *testing.T) {
	d1 := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	entries := []LogEntry{
		{Date: d1, Kind: "Creation", Title: "Alpha", Path: "kb/a/x/1.md"},
		{Date: d2, Kind: "Update", Title: "Beta", Path: "kb/b/y/2.md"},
		{Date: d2, Kind: "Creation", Title: "Gamma", Path: "kb/c/z/3.md"},
	}
	got := string(RenderLog(entries))
	want := "# Log\n\n" +
		"## 2026-07-22\n\n" +
		"- **Creation** Gamma\n" +
		"- **Update** Beta\n\n" +
		"## 2026-07-20\n\n" +
		"- **Creation** Alpha\n"
	if got != want {
		t.Fatalf("RenderLog mismatch:\n got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderLog_Empty(t *testing.T) {
	got := string(RenderLog(nil))
	if got != "# Log\n\nNo changes recorded.\n" {
		t.Fatalf("empty log wrong: %q", got)
	}
}
