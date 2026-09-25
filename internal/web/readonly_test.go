package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
		// The session-bound (unscoped) MCP mount is the same POST-for-reads shape.
		// Gating it would 403 initialize and knomit_bind themselves, so a
		// read-only instance could never be read through the no-flag bridge.
		{"POST", "/api/v1/mcp", false},
		{"POST", "/api/v1/mcp/messages", false},
		{"DELETE", "/api/v1/mcp", false},
		// ...but the bare-/mcp alternative must be anchored just like the others:
		// a path that merely CONTAINS /mcp, or starts with "mcp", stays gated.
		{"POST", "/api/v1/mcpfoo", true},
		{"POST", "/api/v1/repos/core/facts/mcp", true},
		// Lens REST CRUD must stay gated — only the /mcp subtree bypasses.
		{"POST", "/api/v1/lenses", true},
		{"PATCH", "/api/v1/lenses/myview", true},
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

// Both read-only router tests carry a REAL (empty) Manager, never a nil one.
// See TestReadOnlyRouter_FactRouteBypassRegression for why that is a heap-
// safety requirement on Windows and not a nicety.
func TestReadOnlyRouter_GatesMutations(t *testing.T) {
	s := &Server{ReadOnly: true, Manager: newTestManagerWithRepos(t)}
	h := s.Handler()

	post := httptest.NewRecorder()
	h.ServeHTTP(post, fromLoopback(httptest.NewRequest("POST", "/api/v1/repos", nil)))
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
//
// THE MANAGER MUST NOT BE NIL, and the reason is not test hygiene. The two
// non-gated MCP requests below pass the gate on purpose and reach the repo
// and lens middleware, which lock the Manager. With a nil Manager that is a
// hardware fault on every OS; on unix the kernel takes it on the signal stack
// and a recovered panic is all that remains, but on windows/amd64 the
// exception is dispatched ON THE GOROUTINE'S OWN STACK, an ~11.6 KiB frame on
// AMX-capable Intel hosts against Go's 4 KiB reserve, and it overruns into
// the heap span beneath (golang/go#81238, open; no released Go fixes it). A
// LATER GC then dies with "found pointer to free object". That is
// knomit#279: five Windows CI runs crashed right after this test's two
// faults, on unrelated PRs. An empty Manager answers 404 (repo) and 503
// (lens registry not started) instead, and those exact codes are asserted:
// any drift that lets either request reach a nil dependency again shows up
// as a recovered-panic 500 here, and the Windows fault guard in tests.yml
// names it.
func TestReadOnlyRouter_FactRouteBypassRegression(t *testing.T) {
	s := &Server{ReadOnly: true, Manager: newTestManagerWithRepos(t)}
	h := s.Handler()

	// The exploit path: PUT to a fact URL whose key happens to contain
	// /branches/evil/mcp — matched the old unanchored regex and bypassed the gate.
	put := httptest.NewRecorder()
	h.ServeHTTP(put, fromLoopback(httptest.NewRequest("PUT",
		"/api/v1/repos/core/branches/main/facts/x/branches/evil/mcp", nil)))
	if put.Code != http.StatusForbidden {
		t.Errorf("PUT exploit path: got status %d, want 403 (bypass must be closed)", put.Code)
	}

	// Confirm we did not over-correct: a legitimate MCP dispatch POST must still
	// bypass the gate (read-only enforcement for MCP is done inside mcp.NewServer).
	// With an empty Manager the request passes the gate and RepoMiddleware
	// answers 404 for the unknown repo: exact, so a 403 (gate regression) and a
	// 500 (a recovered panic on the way, see the comment above) both fail.
	mcp := httptest.NewRecorder()
	h.ServeHTTP(mcp, fromLoopback(httptest.NewRequest("POST",
		"/api/v1/repos/core/branches/main/mcp", nil)))
	// The title pins WHICH 404: chi answers 404 for a route that no longer
	// exists too, and that would be a different regression.
	if mcp.Code != http.StatusNotFound || !strings.Contains(mcp.Body.String(), "Repo not found") {
		t.Errorf("POST legitimate MCP path: got %d %q, want 404 \"Repo not found\" from RepoMiddleware (403 = gate blocks MCP; 500 = a nil dependency was reached)", mcp.Code, mcp.Body.String())
	}

	// Lens-scoped MCP dispatch is also POST-for-reads and must not be gated by
	// method (regression: the branch-only regex 403'd lens MCP on read-only).
	// The lens middleware answers 503 while the registry is not started.
	lensMCP := httptest.NewRecorder()
	h.ServeHTTP(lensMCP, fromLoopback(httptest.NewRequest("POST", "/api/v1/lenses/myview/mcp", nil)))
	if lensMCP.Code != http.StatusServiceUnavailable {
		t.Errorf("POST lens MCP path: got %d, want 503 from the lens middleware (403 = gate blocks lens MCP; 500 = a nil dependency was reached)", lensMCP.Code)
	}

	// But the lens REST CRUD must stay gated in read-only mode.
	lensDelete := httptest.NewRecorder()
	h.ServeHTTP(lensDelete, fromLoopback(httptest.NewRequest("DELETE", "/api/v1/lenses/myview", nil)))
	if lensDelete.Code != http.StatusForbidden {
		t.Errorf("DELETE lens REST path: got status %d, want 403 (CRUD must stay gated)", lensDelete.Code)
	}

	// PATCH (mount/write/description edit) is a mutating REST route and must be
	// gated just like POST/DELETE.
	lensPatch := httptest.NewRecorder()
	h.ServeHTTP(lensPatch, fromLoopback(httptest.NewRequest("PATCH", "/api/v1/lenses/myview", nil)))
	if lensPatch.Code != http.StatusForbidden {
		t.Errorf("PATCH lens REST path: got status %d, want 403 (CRUD must stay gated)", lensPatch.Code)
	}
}
