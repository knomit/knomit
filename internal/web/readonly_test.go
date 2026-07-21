package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsMutatingRequest(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/repos/core/branches/main/facts/x", false},
		{"HEAD", "/api/v1/version", false},
		{"OPTIONS", "/api/v1/repos", false},
		{"POST", "/api/v1/repos", true},
		{"PUT", "/api/v1/repos/core/branches/main/facts/x", true},
		{"PATCH", "/api/v1/repos/core/origin/upstream", true},
		{"DELETE", "/api/v1/repos/core/origin", true},
		// MCP dispatch is POST-for-reads — never gated by method.
		{"POST", "/api/v1/repos/core/branches/main/mcp", false},
		{"POST", "/api/v1/repos/core/branches/main/mcp/messages", false},
		// Lens-scoped MCP dispatch is the same POST-for-reads shape and must also
		// bypass the method gate (regression: lens MCP was 403'd on read-only).
		{"POST", "/api/v1/lenses/myview/mcp", false},
		{"POST", "/api/v1/lenses/myview/mcp/messages", false},
		// Lens REST CRUD must stay gated — only the /mcp subtree bypasses.
		{"POST", "/api/v1/lenses", true},
		{"DELETE", "/api/v1/lenses/myview", true},
		// A fact whose name ends in "mcp" must still be gated (not the MCP route).
		{"PUT", "/api/v1/repos/core/branches/main/facts/kb/x/mcp", true},
		// Regression: crafted fact paths containing /branches/X/mcp must be gated
		// (these exploited the old unanchored regex to bypass the 403 gate).
		{"PUT", "/api/v1/repos/core/branches/main/facts/x/branches/evil/mcp", true},
		{"DELETE", "/api/v1/repos/core/branches/main/facts/x/branches/evil/mcp", true},
		// Same anchoring for the lens alternative: a crafted fact path containing a
		// /lenses/X/mcp segment must NOT bypass the gate.
		{"PUT", "/api/v1/repos/core/branches/main/facts/x/lenses/evil/mcp", true},
		{"POST", "/api/v1/repos/core/facts/lenses/x/mcp", true},
	}
	for _, c := range cases {
		if got := isMutatingRequest(c.method, c.path); got != c.want {
			t.Errorf("isMutatingRequest(%q,%q)=%v want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestReadOnlyRouter_GatesMutations(t *testing.T) {
	s := &Server{ReadOnly: true}
	h := s.Handler()

	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest("POST", "/api/v1/repos", nil))
	if post.Code != http.StatusForbidden {
		t.Fatalf("POST /repos status = %d, want 403", post.Code)
	}

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/api/v1/version", nil))
	if get.Code == http.StatusForbidden {
		t.Fatal("GET /version must not be gated in read-only mode")
	}
}

// TestReadOnlyRouter_FactRouteBypassRegression is the authoritative end-to-end
// regression test for the unanchored-regex bypass: a crafted fact path that
// contains a /branches/X/mcp segment must NOT bypass the read-only gate.
func TestReadOnlyRouter_FactRouteBypassRegression(t *testing.T) {
	s := &Server{ReadOnly: true}
	h := s.Handler()

	// The exploit path: PUT to a fact URL whose key happens to contain
	// /branches/evil/mcp — matched the old unanchored regex and bypassed the gate.
	put := httptest.NewRecorder()
	h.ServeHTTP(put, httptest.NewRequest("PUT",
		"/api/v1/repos/core/branches/main/facts/x/branches/evil/mcp", nil))
	if put.Code != http.StatusForbidden {
		t.Errorf("PUT exploit path: got status %d, want 403 (bypass must be closed)", put.Code)
	}

	// Confirm we did not over-correct: a legitimate MCP dispatch POST must still
	// bypass the gate (read-only enforcement for MCP is done inside mcp.NewServer).
	// We only assert it is NOT 403; the actual status depends on the MCP handler.
	mcp := httptest.NewRecorder()
	h.ServeHTTP(mcp, httptest.NewRequest("POST",
		"/api/v1/repos/core/branches/main/mcp", nil))
	if mcp.Code == http.StatusForbidden {
		t.Errorf("POST legitimate MCP path: got 403, want non-403 (gate must not block MCP)")
	}

	// Lens-scoped MCP dispatch is also POST-for-reads and must not be gated by
	// method (regression: the branch-only regex 403'd lens MCP on read-only).
	lensMCP := httptest.NewRecorder()
	h.ServeHTTP(lensMCP, httptest.NewRequest("POST", "/api/v1/lenses/myview/mcp", nil))
	if lensMCP.Code == http.StatusForbidden {
		t.Errorf("POST lens MCP path: got 403, want non-403 (gate must not block lens MCP)")
	}

	// But the lens REST CRUD must stay gated in read-only mode.
	lensDelete := httptest.NewRecorder()
	h.ServeHTTP(lensDelete, httptest.NewRequest("DELETE", "/api/v1/lenses/myview", nil))
	if lensDelete.Code != http.StatusForbidden {
		t.Errorf("DELETE lens REST path: got status %d, want 403 (CRUD must stay gated)", lensDelete.Code)
	}
}
