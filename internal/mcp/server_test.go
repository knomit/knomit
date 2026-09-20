package mcp

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func toolNames(regs []toolReg) []string {
	names := make([]string, len(regs))
	for i, r := range regs {
		names[i] = r.tool.Name
	}
	sort.Strings(names)
	return names
}

func TestEnabledTools_ReadOnly_OmitsWriteTools(t *testing.T) {
	got := toolNames(enabledTools(toolRegistrations(nil), true))
	// knomit_bind is NOT a write tool: a read-only server still needs it on the
	// unscoped mount, or nothing there could be read either.
	want := []string{"knomit_bind", "knomit_explain", "knomit_query", "knomit_repos"}
	require.Equal(t, want, got)
}

func TestEnabledTools_Writable_IncludesAllTen(t *testing.T) {
	if n := len(enabledTools(toolRegistrations(nil), false)); n != 10 {
		t.Fatalf("writable tool count = %d, want 10", n)
	}
}

// knomit_experiment is a WRITE tool even though it authors no fact: open
// forks a branch, commit merges one into the agent branch, rollback deletes
// one. A read-only demo instance must not expose it, and the read-only list
// above is where that is asserted — this names the reason so the pairing is
// not mistaken for an oversight.
func TestEnabledTools_ReadOnly_OmitsExperiment(t *testing.T) {
	require.NotContains(t, toolNames(enabledTools(toolRegistrations(nil), true)), "knomit_experiment")
	require.Contains(t, toolNames(enabledTools(toolRegistrations(nil), false)), "knomit_experiment")
}
