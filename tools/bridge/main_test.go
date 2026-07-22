package main

import (
	"strings"
	"testing"
)

// TestLensConflict covers the mutual-exclusion guard: --lens rejects an
// explicitly-set --repo. Since --repo carries a non-empty default, the caller
// passes an explicit-set bool (from flag.Visit), never a value comparison.
func TestLensConflict(t *testing.T) {
	cases := []struct {
		name    string
		lens    string
		repoSet bool
		wantSub string // "" means no conflict
	}{
		{"no lens, repo set", "", true, ""},
		{"lens alone", "eng", false, ""},
		{"lens + repo", "eng", true, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lensConflict(tc.lens, tc.repoSet)
			if tc.wantSub == "" {
				if got != "" {
					t.Errorf("lensConflict = %q, want no conflict", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("lensConflict = %q, want substring %q", got, tc.wantSub)
			}
		})
	}
}

func TestMcpURL_RepoMode(t *testing.T) {
	got := mcpURL("http://localhost:19278", "core", "", "agent:host")
	want := "http://localhost:19278/api/v1/repos/core/branches/agent:host/mcp"
	if got != want {
		t.Errorf("mcpURL repo mode = %q, want %q", got, want)
	}
}

func TestMcpURL_LensMode(t *testing.T) {
	// A lens has no branch segment (LensMiddleware resolves each mount's
	// branch server-side).
	got := mcpURL("http://localhost:19278", "", "eng", "")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens mode = %q, want %q", got, want)
	}
}

// mcpURL must ignore repo/branch when a lens is set — a lens URL never carries
// a branch even if that arg is non-empty.
func TestMcpURL_LensTakesPrecedence(t *testing.T) {
	got := mcpURL("http://localhost:19278", "core", "eng", "agent:host")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens precedence = %q, want %q", got, want)
	}
}
