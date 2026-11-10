package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
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
		// Closing a factset frees the caller's own read session: not a write.
		// Only DELETE on the factset resource itself; a fact path that ends
		// in /search/factset, the export, and other methods stay gated.
		{"DELETE", "/api/v1/repos/core/branches/main/search/factset", false},
		{"POST", "/api/v1/repos/core/branches/main/search/factset", true},
		{"DELETE", "/api/v1/repos/core/branches/main/search/factset/export", true},
		{"DELETE", "/api/v1/repos/core/branches/main/facts/x/search/factset", true},
		{"DELETE", "/api/v1/repos/core/branches/main/search/factsetx", true},
		// The lens twin: DELETE /lenses/{lens}/search/factset closes the
		// caller's lens factset. The lens itself, the export and the
		// look-alikes stay gated.
		{"DELETE", "/api/v1/lenses/eng/search/factset", false},
		{"POST", "/api/v1/lenses/eng/search/factset", true},
		{"DELETE", "/api/v1/lenses/eng/search/factset/export", true},
		{"DELETE", "/api/v1/lenses/eng/search/factsetx", true},
		{"DELETE", "/api/v1/lenses/eng/facts/search/factset", true},
		{"DELETE", "/api/v1/lenses//search/factset", true},
		// The chat routes create chats, post turns and answer approvals; none is
		// a knowledge-base write (the per-tool permission gate decides what a
		// chat's calls may do), so they bypass the method gate...
		{"POST", "/api/v1/chats", false},
		{"POST", "/api/v1/chats/chat-0f/turns", false},
		{"POST", "/api/v1/chats/chat-0f/approvals/tu_1", false},
		{"DELETE", "/api/v1/chats/chat-0f", false},
		{"DELETE", "/api/v1/chats/chat-0f/grants", false},
		// ...but the pattern is anchored: look-alikes and paths that merely
		// contain /chats stay gated.
		{"POST", "/api/v1/chatsfoo", true},
		{"POST", "/api/v1/chat-providers", true},
		{"PUT", "/api/v1/repos/core/branches/main/facts/x/chats/y", true},
		{"POST", "/api/v1/repos/core/chats", true},
		// POST /facts/resolve only reads (it looks fact handles up), so it is
		// not a write; the pattern is anchored on both ends.
		{"POST", "/api/v1/facts/resolve", false},
		{"POST", "/api/v1/facts/resolvex", true},
		{"POST", "/api/v1/facts/resolve/x", true},
		{"DELETE", "/api/v1/facts/resolve", true},
		{"PUT", "/api/v1/facts/resolve", true},
		{"POST", "/api/v1/repos/core/branches/main/facts/resolve", true},
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
	h.ServeHTTP(post, asAnonymousOnLoopback(httptest.NewRequest("POST", "/api/v1/repos", nil)))
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
	h.ServeHTTP(put, asAnonymousOnLoopback(httptest.NewRequest("PUT",
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
	h.ServeHTTP(mcp, asAnonymousOnLoopback(httptest.NewRequest("POST",
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
	h.ServeHTTP(lensMCP, asAnonymousOnLoopback(httptest.NewRequest("POST", "/api/v1/lenses/myview/mcp", nil)))
	if lensMCP.Code != http.StatusServiceUnavailable {
		t.Errorf("POST lens MCP path: got %d, want 503 from the lens middleware (403 = gate blocks lens MCP; 500 = a nil dependency was reached)", lensMCP.Code)
	}

	// But the lens REST CRUD must stay gated in read-only mode.
	lensDelete := httptest.NewRecorder()
	h.ServeHTTP(lensDelete, asAnonymousOnLoopback(httptest.NewRequest("DELETE", "/api/v1/lenses/myview", nil)))
	if lensDelete.Code != http.StatusForbidden {
		t.Errorf("DELETE lens REST path: got status %d, want 403 (CRUD must stay gated)", lensDelete.Code)
	}

	// PATCH (mount/write/description edit) is a mutating REST route and must be
	// gated just like POST/DELETE.
	lensPatch := httptest.NewRecorder()
	h.ServeHTTP(lensPatch, asAnonymousOnLoopback(httptest.NewRequest("PATCH", "/api/v1/lenses/myview", nil)))
	if lensPatch.Code != http.StatusForbidden {
		t.Errorf("PATCH lens REST path: got status %d, want 403 (CRUD must stay gated)", lensPatch.Code)
	}
}

// A principal without the write permission can start a chat and answer its
// approvals: writeGate lets the chat routes through, because the permission to
// write is checked per TOOL when a chat's call reaches the MCP server. A real
// write route stays refused for the same principal.
//
// SABOTAGE: drop chatRoutePattern from isMutatingRequest -> red (403).
func TestWriteGate_LetsAReadOnlyPrincipalUseChat(t *testing.T) {
	reader := auth.Principal{Kind: auth.KindBridge, ID: "uid:777", Via: auth.ViaSocket}
	g := auth.StaticGrants{reader.String(): {auth.Read: {}}}
	reached := 0
	h := writeGate(g, false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), reader))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do("POST", "/api/v1/chats"); got != http.StatusNoContent {
		t.Errorf("POST /chats as a read-only principal = %d, want 204", got)
	}
	if got := do("POST", "/api/v1/chats/chat-0f/approvals/tu_1"); got != http.StatusNoContent {
		t.Errorf("POST approval as a read-only principal = %d, want 204", got)
	}
	if got := do("POST", "/api/v1/repos"); got != http.StatusForbidden {
		t.Errorf("POST /repos as a read-only principal = %d, want 403", got)
	}
	if reached != 2 {
		t.Errorf("handler reached %d times, want 2", reached)
	}
}

// A read-only (demo) server does not mount the chat at all: the routes answer
// 404, with a provider registry configured or not.
func TestHandler_ReadOnly_DoesNotMountChat(t *testing.T) {
	s := &Server{ReadOnly: true, APIOnly: true, Manager: newTestManagerWithRepos(t), LLMProviders: llm.NewRegistry(nil)}
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/chats"},
		{"GET", "/api/v1/chats"},
		{"GET", "/api/v1/chat-providers"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asOwner(httptest.NewRequest(tc.method, tc.path, nil)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("read-only %s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// And a writable server does: without a provider registry the list is empty
// and starting a chat is refused, rather than the route being absent.
func TestHandler_Writable_MountsChat(t *testing.T) {
	s := &Server{APIOnly: true, Manager: newTestManagerWithRepos(t)}
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asOwner(httptest.NewRequest("GET", "/api/v1/chat-providers", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /chat-providers = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"providers":[]`) {
		t.Errorf("no registry should list no providers: %s", rec.Body)
	}
}

// POST /facts/resolve is a read: a principal holding only Read gets through
// the write gate, and a read-only (demo) server serves it.
func TestFactsResolve_IsARead(t *testing.T) {
	reader := auth.Principal{Kind: auth.KindBridge, ID: "uid:777", Via: auth.ViaSocket}
	g := auth.StaticGrants{reader.String(): {auth.Read: {}}}
	reached := 0
	h := writeGate(g, false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("POST", "/api/v1/facts/resolve", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), reader))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || reached != 1 {
		t.Fatalf("POST /facts/resolve as a read-only principal = %d (reached %d), want 204", rec.Code, reached)
	}

	s := &Server{ReadOnly: true, APIOnly: true, Manager: newTestManagerWithRepos(t)}
	rec = httptest.NewRecorder()
	r := asAnonymousOnLoopback(httptest.NewRequest("POST", "/api/v1/facts/resolve", strings.NewReader(`{"handles":["deadbeef"]}`)))
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("read-only server POST /facts/resolve = %d %s, want 200 []", rec.Code, rec.Body)
	}
}
