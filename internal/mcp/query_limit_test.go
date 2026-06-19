package mcp

import "testing"

// TestPageSizeFor regresses the page-size bounding for knomit_query (descends
// from PR #70 finding #2: an unbounded MCP limit could materialise the whole
// corpus into one tool response). The page size is now mode-dependent: snippet
// pages are bounded by maxPageSize, and include_body pages by the much smaller
// includeBodyMaxPage since full bodies are heavy.
func TestPageSizeFor(t *testing.T) {
	cases := []struct {
		name        string
		in          int
		includeBody bool
		want        int
	}{
		{"snippet: zero falls back to default", 0, false, defaultPageSize},
		{"snippet: negative falls back to default", -7, false, defaultPageSize},
		{"snippet: in-range passes through", 42, false, 42},
		{"snippet: at cap passes through", maxPageSize, false, maxPageSize},
		{"snippet: above cap is clamped", maxPageSize + 1, false, maxPageSize},
		{"snippet: absurd value is clamped", 10_000_000, false, maxPageSize},
		{"include_body: zero falls back to default", 0, true, includeBodyDefaultPage},
		{"include_body: in-range passes through", 4, true, 4},
		{"include_body: above cap is clamped", includeBodyMaxPage + 1, true, includeBodyMaxPage},
		{"include_body: absurd value is clamped", 999, true, includeBodyMaxPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageSizeFor(tc.in, tc.includeBody); got != tc.want {
				t.Fatalf("pageSizeFor(%d, %v) = %d, want %d", tc.in, tc.includeBody, got, tc.want)
			}
		})
	}
}
