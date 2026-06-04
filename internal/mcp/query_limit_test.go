package mcp

import "testing"

// TestClampQueryLimit regresses PR #70 review finding #2: knomit_query used to
// clamp only the lower bound, leaving the result limit unbounded above while the
// REST search handler caps it at 500. An unbounded MCP limit could materialise
// the whole corpus into one tool response. clampQueryLimit now mirrors REST.
func TestClampQueryLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"absent/zero falls back to default", 0, defaultQueryLimit},
		{"negative falls back to default", -7, defaultQueryLimit},
		{"in-range value passes through", 42, 42},
		{"at cap passes through", maxQueryLimit, maxQueryLimit},
		{"above cap is clamped", maxQueryLimit + 1, maxQueryLimit},
		{"absurd value is clamped", 10_000_000, maxQueryLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampQueryLimit(tc.in); got != tc.want {
				t.Fatalf("clampQueryLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
