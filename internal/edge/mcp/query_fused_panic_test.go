package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// The intended behaviour change of the fused-search extraction (PR #704).
//
// Before it, knomit_query's fan-out armed its recover with
// `defer recoverFanout(mountLabel(t.RT), ...)`: the label (rt.RI.Name()) was
// evaluated BEFORE the recover existed, so a mount whose RepoInstance is nil
// panicked in a bare goroutine and took the whole process down. The shared
// federate.FanOut builds the label before the goroutine without dereferencing
// the instance, so the same mount is now an ordinary tool error naming its
// branch, through the "any mount error fails the whole query" path (RFC §9.1).
func TestQueryFused_NilInstanceMountPanicIsAToolError(t *testing.T) {
	write := fusedRepo(t, "mid")
	b := repos.NewBindingForTest(write,
		repos.ReadTarget{RI: write, Branch: "agent/test"},
		repos.ReadTarget{RI: nil, Branch: "gone/branch"},
	)

	for _, sort := range []string{"relevance", "recent"} {
		t.Run(sort, func(t *testing.T) {
			result, text := queryVia(t, b, map[string]any{"type": []any{"observation"}, "sort": sort})
			require.Truef(t, result.IsError, "a panicking mount must fail the query: %s", text)
			prefix := "search error: "
			if sort == "recent" {
				prefix = "recent error: "
			}
			require.Containsf(t, text, prefix+"mount gone/branch panicked: ", "got %q", text)
		})
	}
}
