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

func TestEnabledTools_Writable_IncludesAllNine(t *testing.T) {
	if n := len(enabledTools(toolRegistrations(nil), false)); n != 9 {
		t.Fatalf("writable tool count = %d, want 9", n)
	}
}
