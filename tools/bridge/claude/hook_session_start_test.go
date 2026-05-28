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
