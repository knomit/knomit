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

// TestSessionStart_LensConfigured_UsesWriteRepo stands up a fake server serving
// the lens resource plus the WRITE repo's agent_branch + facts. The hook must
// resolve the lens to its write repo and scope every repo query to that repo —
// never to the (unrelated) project-directory basename.
func TestSessionStart_LensConfigured_UsesWriteRepo(t *testing.T) {
	dir := t.TempDir()
	writeLensMCP(t, dir, "mylens")

	var lensHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/lenses/", func(w http.ResponseWriter, r *http.Request) {
		lensHit = true
		w.Header().Set("Content-Type", "application/hal+json")
		w.Write([]byte(`{"name":"mylens","write":{"uid":"uid-writerepo","name":"writerepo"},"reads":[]}`))
	})
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		// The basename must never leak into a repo query; only the lens's
		// write repo is allowed.
		if !strings.Contains(r.URL.Path, "/repos/writerepo") {
			t.Errorf("repo query scoped to %q, want /repos/writerepo", r.URL.Path)
		}
		switch {
		case strings.Contains(r.URL.Path, "/facts"):
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/principles/philosophy/write-scope/a.md","title":"Write-repo principle","domain":["global"],"entities":["designer"]}
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

	require.True(t, lensHit, "lens resource was never queried")
	require.Contains(t, out.String(), "Write-repo principle")
}

// TestSessionStart_LensUnresolved_CleanNoOp verifies that a lens-configured dir
// with an unreachable server produces a clean no-op (no output) rather than
// falling back to the basename — the dangerous wrong-repo failure mode B.6
// closes.
func TestSessionStart_LensUnresolved_CleanNoOp(t *testing.T) {
	dir := t.TempDir()
	writeLensMCP(t, dir, "mylens")
	closedKnomit(t)

	payload := map[string]interface{}{
		"cwd":             dir,
		"session_id":      "s1",
		"transcript_path": "/tmp/nope.jsonl",
	}
	data, _ := json.Marshal(payload)

	var out bytes.Buffer
	require.NoError(t, hookSessionStart(bytes.NewReader(data), &out))
	require.Zero(t, out.Len(), "expected clean no-op when lens is unresolved")
}

// TestSessionStart_EmitsAvailableOnDemandTOC asserts that after the
// PROJECT PRINCIPLES block, the hook emits an "AVAILABLE ON DEMAND" line
// summarizing per-area fact counts grouped by the SECOND path segment
// under kb/. Global principles (kb/principles/* with domain=global) are
// excluded from the TOC because they're already rendered above; scoped
// principles (no global domain) ARE included, grouped by bucket (e.g.
// "anti-patterns"). Areas are listed alphabetically.
// TestSessionStart_OmitsAreaTOC pins the REMOVAL of the "AVAILABLE ON DEMAND"
// block. It counted areas over the 200-most-recent-facts window
// (?sort=recent&limit=200), so its per-area numbers read as corpus depth while
// actually measuring recent write activity: every count in a real emitted block
// summed to 199. A quiet, well-covered area looked small and a churny one
// looked large, and a two-month-old load-bearing invariant did not appear at
// all. It also told the reader to run "/knomit-recall <area>" while listing
// ontology TOPICS, which is a different axis from the domain `applies_to`
// matches — see the knomit-recall skill.
//
// Nothing replaces it for main sessions: the CLAUDE.md knomit block already
// states what knomit holds and when to recall, and duplicating CLAUDE.md
// guidance in a hook is one of the reasons dd5f32ba deleted three others.
func TestSessionStart_OmitsAreaTOC(t *testing.T) {
	dir := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/facts"):
			w.Header().Set("Content-Type", "application/hal+json")
			w.Write([]byte(`{"_embedded":{"facts":[
				{"path":"kb/invariants/store/a.md","title":"store a","domain":["store"],"entities":[]},
				{"path":"kb/invariants/store/b.md","title":"store b","domain":["store"],"entities":[]},
				{"path":"kb/decisions/mcp/e.md","title":"mcp decision","domain":["mcp"],"entities":[]}
			]}}`))
		default:
			w.Write([]byte(`{"agent_branch":"machine/test"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)

	data, _ := json.Marshal(map[string]interface{}{
		"cwd": dir, "session_id": "s1", "transcript_path": "/tmp/nope.jsonl",
	})
	var out bytes.Buffer
	require.NoError(t, hookSessionStart(bytes.NewReader(data), &out))

	got := out.String()
	require.NotContains(t, got, "AVAILABLE ON DEMAND")
	require.NotContains(t, got, "/knomit-recall <area>")
	// The recency-window counts specifically must not come back.
	require.NotContains(t, got, "store (2)")
	require.NotContains(t, got, "mcp (1)")
	// The rest of the block still works.
	require.Contains(t, got, "Known facts from knomit for this codebase:")
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
