package endpoint

import "testing"

func TestServerURLEncodesBranch(t *testing.T) {
	got := ServerURL("http://localhost:19278", "core", "agent/main", "code")
	want := "http://localhost:19278/api/v1/repos/core/branches/agent:main/mcp?profile=code"
	if got != want {
		t.Fatalf("ServerURL = %q, want %q", got, want)
	}
}
