package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/oauth"
)

// N6 (3a review, reviewer S10 in 3b): the MCP HTTPContextFunc copies the
// bearer CEILING as well as the principal. At mcp-go v0.45.0 neither copy is
// load-bearing — the POST path builds its ctx from r.Context(), and the
// task-augmented path runs on that same ctx — so the only test that can go
// red on the missing ceiling is a unit test of the function itself, fed a
// context that does NOT descend from the request. That is this test.
func TestMCPHTTPContext_CopiesPrincipalAndCeiling(t *testing.T) {
	p := oauth.TokenPrincipal("laptop")
	ceiling := auth.Set{auth.Read: {}}
	r := httptest.NewRequest("POST", alphaMCP, nil)
	r = r.WithContext(auth.WithCeiling(auth.WithPrincipal(r.Context(), p), ceiling))

	ctx := mcpHTTPContext(context.Background(), r)
	if got, ok := auth.FromContext(ctx); !ok || got != p {
		t.Fatalf("principal not copied: %v %v", got, ok)
	}
	got, ok := auth.CeilingFromContext(ctx)
	if !ok || len(got) != 1 || !got.Has(auth.Read) {
		t.Fatalf("ceiling not copied: %v %v — a future mcp-go that builds this ctx from scratch would make every bearer call fail closed", got, ok)
	}
	// A request with no ceiling (every non-bearer principal) gains none.
	r2 := httptest.NewRequest("POST", alphaMCP, nil)
	if _, ok := auth.CeilingFromContext(mcpHTTPContext(context.Background(), r2)); ok {
		t.Fatal("a ceiling appeared from nowhere")
	}
}

// callToolAsTask sends a task-augmented tools/call (params.task set) and
// returns the tool result's text, fetched with tasks/result. This is the
// dispatch path on which mcp-go applies NO tool middleware
// (kb/invariants/mcp/binding-gate/registration-seam).
func callToolAsTask(t *testing.T, h http.Handler, sid, name, args string) string {
	t.Helper()
	resp, _ := rpcAt(t, h, alphaMCP, sid, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":%q,"arguments":%s,"task":{"ttl":60000}}}`, name, args))
	res, _ := resp["result"].(map[string]any)
	task, _ := res["task"].(map[string]any)
	id, _ := task["taskId"].(string)
	if id == "" {
		t.Fatalf("not a task-augmented dispatch (no taskId): %v", resp)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := rpcAt(t, h, alphaMCP, sid, fmt.Sprintf(`{"jsonrpc":"2.0","id":22,"method":"tasks/result","params":{"taskId":%q}}`, id))
		if r, ok := out["result"].(map[string]any); ok {
			raw := fmt.Sprint(r["content"])
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never completed: %v", id, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Regression pin, NOT red-to-green (S10): a read-ceiling bearer cannot run
// a WRITE tool through the task-augmented path. It passes with or without
// the ceiling copy, because the task ctx descends from the request; what it
// pins is that the permission gate sits at registration, the one seam both
// dispatch paths share. The positive twin below shows the path is live and
// reaches the handler, so the refusal is the gate's and not a dead route.
//
// Sabotage (run 2026-09-24): removing gatePermission from the AddTool loop
// in internal/mcp/server.go makes the read token's call succeed past the
// gate, and this fails.
func TestOAuthMCP_TaskPathRespectsTheCeiling(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	for _, tc := range []struct {
		ceiling []string
		denied  bool
	}{
		{[]string{"read"}, true},
		{[]string{"read", "write"}, false},
	} {
		t.Run(strings.Join(tc.ceiling, "+"), func(t *testing.T) {
			tok := f.mint(t, "laptop-"+strings.Join(tc.ceiling, ""), testIssuer, tc.ceiling, auth.Read, auth.Write)
			h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
			_, sid := rpcAt(t, h, alphaMCP, "", initBody)
			text := callToolAsTask(t, h, sid, "knomit_review", `{}`)
			if got := strings.Contains(text, "permission denied"); got != tc.denied {
				t.Fatalf("ceiling %v: permission denied = %v, want %v; result: %s", tc.ceiling, got, tc.denied, text)
			}
		})
	}
}
