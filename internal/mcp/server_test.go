package mcp

import (
	"sort"
	"testing"
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
	got := toolNames(enabledTools(toolRegistrations(), true))
	want := []string{"knomit_explain", "knomit_query", "knomit_repos"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("read-only tools = %v, want %v", got, want)
	}
}

func TestEnabledTools_Writable_IncludesAllEight(t *testing.T) {
	if n := len(enabledTools(toolRegistrations(), false)); n != 8 {
		t.Fatalf("writable tool count = %d, want 8", n)
	}
}
