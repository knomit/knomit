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
		{"GET", "/repos/core/branches/main/facts/x", false},
		{"HEAD", "/version", false},
		{"OPTIONS", "/repos", false},
		{"POST", "/repos", true},
		{"PUT", "/repos/core/branches/main/facts/x", true},
		{"PATCH", "/repos/core/origin/upstream", true},
		{"DELETE", "/repos/core/origin", true},
		// MCP dispatch is POST-for-reads — never gated by method.
		{"POST", "/repos/core/branches/main/mcp", false},
		{"POST", "/repos/core/branches/main/mcp/messages", false},
		// A fact whose name ends in "mcp" must still be gated (not the MCP route).
		{"PUT", "/repos/core/branches/main/facts/kb/x/mcp", true},
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
