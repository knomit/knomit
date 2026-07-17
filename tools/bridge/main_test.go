package main

import "testing"

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
