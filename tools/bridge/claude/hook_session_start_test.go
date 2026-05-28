package claude

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionStart_EmitsGlobalPrinciples stands up a fake knomit HTTP server
// that serves agent_branch + a fixed recent-facts list, then asserts the
// hook output contains a PROJECT PRINCIPLES block sourced from
// kb/principles/* facts whose domain contains "global" and entities contain
// "designer". Task 15 will add a fallback to invariants when no globals
// exist; for this task, the invariants block should be gone entirely.
func TestSessionStart_EmitsGlobalPrinciples(t *testing.T) {
	dir := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/facts"):
			// Two principle facts that should pass the global+designer
			// filter, plus one invariant fact that must NOT surface in
			// the principles block.
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/principles/philosophy/historical-graph/a.md","title":"Historical graph, never HEAD","domain":["global"],"entities":["designer"]},
				{"path":"kb/principles/ux/agent-voice/b.md","title":"Terse over comprehensive","domain":["global"],"entities":["designer"]},
				{"path":"kb/invariants/store/c.md","title":"Vtables must not re-enter","domain":["store"],"entities":[]}
			]}}`))
		default:
			// agentBranch lookup: GET /api/v1/repos/{repo}
			w.Write([]byte(`{"agent_branch":"machine/test"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	payload := map[string]interface{}{
		"cwd":             dir,
		"session_id":      "s1",
		"transcript_path": "/tmp/nope.jsonl",
	}
	data, _ := json.Marshal(payload)

	var out bytes.Buffer
	require.NoError(t, hookSessionStart(bytes.NewReader(data), &out))

	got := out.String()
	require.Contains(t, got, "PROJECT PRINCIPLES:")
	require.Contains(t, got, "Historical graph, never HEAD")
	require.Contains(t, got, "Terse over comprehensive")
	require.NotContains(t, got, "LOAD-BEARING INVARIANTS:")
	// Bullet glyph and bucket/slug short path:
	require.Contains(t, got, "philosophy/historical-graph")
	require.Contains(t, got, "ux/agent-voice")
}

// TestSessionStart_EmitsAvailableOnDemandTOC asserts that after the
// PROJECT PRINCIPLES block, the hook emits an "AVAILABLE ON DEMAND" line
// summarizing per-area fact counts grouped by the SECOND path segment
// under kb/. Global principles (kb/principles/* with domain=global) are
// excluded from the TOC because they're already rendered above; scoped
// principles (no global domain) ARE included, grouped by bucket (e.g.
// "anti-patterns"). Areas are listed alphabetically.
func TestSessionStart_EmitsAvailableOnDemandTOC(t *testing.T) {
	dir := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/facts"):
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/invariants/store/a.md","title":"store a","domain":["store"],"entities":[]},
				{"path":"kb/invariants/store/b.md","title":"store b","domain":["store"],"entities":[]},
				{"path":"kb/invariants/ui/c.md","title":"ui c","domain":["ui"],"entities":[]},
				{"path":"kb/principles/anti-patterns/bridge/d.md","title":"bridge anti-pattern","domain":["bridge"],"entities":["designer"]},
				{"path":"kb/decisions/mcp/e.md","title":"mcp decision","domain":["mcp"],"entities":[]}
			]}}`))
		default:
			w.Write([]byte(`{"agent_branch":"machine/test"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	payload := map[string]interface{}{
		"cwd":             dir,
		"session_id":      "s1",
		"transcript_path": "/tmp/nope.jsonl",
	}
	data, _ := json.Marshal(payload)

	var out bytes.Buffer
	require.NoError(t, hookSessionStart(bytes.NewReader(data), &out))

	got := out.String()
	require.Contains(t, got, "AVAILABLE ON DEMAND (use /knomit-recall <area>):")
	// Per-area counts (alphabetical order enforced by helper, but assert
	// substring presence so reordering individual entries is harmless).
	require.Contains(t, got, "anti-patterns (1)")
	require.Contains(t, got, "mcp (1)")
	require.Contains(t, got, "store (2)")
	require.Contains(t, got, "ui (1)")
}

// TestSessionStart_FallsBackToInvariantsWhenNoGlobalPrinciples verifies the
// rollout safety net: until designers seed global principles, the hook
// continues to emit the legacy LOAD-BEARING INVARIANTS block (top-5
// invariants by prefix match on kb/invariants/). Once any global
// principle is authored, this fallback goes dark — see
// TestSessionStart_EmitsGlobalPrinciples for the steady-state behavior.
func TestSessionStart_FallsBackToInvariantsWhenNoGlobalPrinciples(t *testing.T) {
	dir := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/facts"):
			// ONLY invariants — no principles at all. The fallback must
			// surface the invariant titles in a LOAD-BEARING INVARIANTS
			// block and must NOT emit a PROJECT PRINCIPLES header.
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/invariants/store/a.md","title":"Vtables must not re-enter","domain":["store"],"entities":[]},
				{"path":"kb/invariants/ui/b.md","title":"Refs branch by scheme","domain":["ui"],"entities":[]}
			]}}`))
		default:
			w.Write([]byte(`{"agent_branch":"machine/test"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	payload := map[string]interface{}{
		"cwd":             dir,
		"session_id":      "s1",
		"transcript_path": "/tmp/nope.jsonl",
	}
	data, _ := json.Marshal(payload)

	var out bytes.Buffer
	require.NoError(t, hookSessionStart(bytes.NewReader(data), &out))

	got := out.String()
	require.Contains(t, got, "LOAD-BEARING INVARIANTS:")
	require.Contains(t, got, "Vtables must not re-enter")
	require.Contains(t, got, "Refs branch by scheme")
	require.NotContains(t, got, "PROJECT PRINCIPLES:")
}
