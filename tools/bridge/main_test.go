package main

import (
	"strings"
	"testing"
)

// TestLensConflict covers the mutual-exclusion guard: --lens rejects any
// explicitly-set repo-scoped flag (--repo, --source, --profile). Since --repo
// and --profile carry non-empty defaults, the caller passes explicit-set bools
// (from flag.Visit), never a value comparison.
func TestLensConflict(t *testing.T) {
	cases := []struct {
		name                           string
		lens                           string
		repoSet, sourceSet, profileSet bool
		wantSub                        string // "" means no conflict
	}{
		{"no lens, flags set", "", true, true, true, ""},
		{"lens alone", "eng", false, false, false, ""},
		{"lens + repo", "eng", true, false, false, "mutually exclusive"},
		{"lens + source", "eng", false, true, false, "do not apply to lens mode"},
		{"lens + profile", "eng", false, false, true, "do not apply to lens mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lensConflict(tc.lens, tc.repoSet, tc.sourceSet, tc.profileSet)
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
	got := mcpURL("http://localhost:19278", "core", "", "agent:host", "code")
	want := "http://localhost:19278/api/v1/repos/core/branches/agent:host/mcp?profile=code"
	if got != want {
		t.Errorf("mcpURL repo mode = %q, want %q", got, want)
	}
}

func TestMcpURL_LensMode(t *testing.T) {
	// A lens has no branch segment (LensMiddleware resolves each mount's
	// branch server-side) and no profile.
	got := mcpURL("http://localhost:19278", "", "eng", "", "")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens mode = %q, want %q", got, want)
	}
}

// mcpURL must ignore repo/branch/profile when a lens is set — a lens URL never
// carries a branch or profile even if those args are non-empty.
func TestMcpURL_LensTakesPrecedence(t *testing.T) {
	got := mcpURL("http://localhost:19278", "core", "eng", "agent:host", "code")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens precedence = %q, want %q", got, want)
	}
}
