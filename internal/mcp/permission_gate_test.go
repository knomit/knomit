package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/auth"
)

func readOnlyPrincipalCtx() (context.Context, auth.StaticGrants) {
	g := auth.StaticGrants{"bridge:uid:1@socket": {auth.Read: {}}}
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{Kind: auth.KindBridge, ID: "uid:1", Via: auth.ViaSocket})
	return ctx, g
}

func TestPermissionFilter_HidesWriteToolsFromReadOnlyPrincipal(t *testing.T) {
	ctx, g := readOnlyPrincipalCtx()
	tools := []mcpgo.Tool{{Name: "knomit_query"}, {Name: "knomit_learn"}}
	out := permissionFilter(g, map[string]bool{"knomit_learn": true})(ctx, tools)
	if len(out) != 1 || out[0].Name != "knomit_query" {
		t.Fatalf("filtered = %+v", out)
	}
}

func TestPermissionFilter_KeepsEverythingForAWriter(t *testing.T) {
	g := auth.StaticGrants{"bridge:uid:1@socket": {auth.Read: {}, auth.Write: {}}}
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{Kind: auth.KindBridge, ID: "uid:1", Via: auth.ViaSocket})
	tools := []mcpgo.Tool{{Name: "knomit_query"}, {Name: "knomit_learn"}}
	if out := permissionFilter(g, map[string]bool{"knomit_learn": true})(ctx, tools); len(out) != 2 {
		t.Fatalf("a writer must see every tool: %+v", out)
	}
}

func TestPermissionFilter_NilGrantsIsNoop(t *testing.T) {
	tools := []mcpgo.Tool{{Name: "knomit_learn"}}
	if out := permissionFilter(nil, map[string]bool{"knomit_learn": true})(context.Background(), tools); len(out) != 1 {
		t.Fatal("nil grants must not filter (in-process callers and tests)")
	}
}

func TestGatePermission_DeniesAndNamesPermission(t *testing.T) {
	ctx, g := readOnlyPrincipalCtx()
	called := false
	next := func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		called = true
		return mcpgo.NewToolResultText("ok"), nil
	}
	h := gatePermission(g, mcpgo.Tool{Name: "knomit_learn"}, auth.Write, next)

	res, err := h(ctx, mcpgo.CallToolRequest{})
	if err != nil || !res.IsError || called {
		t.Fatalf("res=%+v err=%v called=%v", res, err, called)
	}
	text := res.Content[0].(mcpgo.TextContent).Text
	if !strings.Contains(text, "write") || !strings.Contains(text, "knomit_learn") {
		t.Fatalf("the error must name the tool AND the permission: %q", text)
	}
	if !strings.Contains(text, "bridge:uid:1@socket") {
		t.Fatalf("the error must name the principal so an operator knows what to grant: %q", text)
	}

	g["bridge:uid:1@socket"] = auth.Set{auth.Read: {}, auth.Write: {}}
	res, _ = h(ctx, mcpgo.CallToolRequest{})
	if res.IsError || !called {
		t.Fatal("granted write must reach the handler")
	}
}

// Filtering is a courtesy; the gate is the check. A caller that never listed
// the tools and calls knomit_learn straight out must still be refused.
func TestGatePermission_NoPrincipalWithGrantsIsDenied(t *testing.T) {
	h := gatePermission(auth.StaticGrants{}, mcpgo.Tool{Name: "knomit_learn"}, auth.Write,
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		})
	res, _ := h(context.Background(), mcpgo.CallToolRequest{})
	if !res.IsError {
		t.Fatal("no principal must be denied when grants are enforced")
	}
}

func TestGatePermission_NilGrantsIsNoop(t *testing.T) {
	called := false
	h := gatePermission(nil, mcpgo.Tool{Name: "knomit_learn"}, auth.Write,
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			called = true
			return mcpgo.NewToolResultText("ok"), nil
		})
	if res, _ := h(context.Background(), mcpgo.CallToolRequest{}); res.IsError || !called {
		t.Fatal("nil grants must not gate")
	}
}

// The permission gate must sit OUTSIDE the binding gate, so a caller with no
// permission is told about the permission rather than about a missing handle
// — and so no handle is resolved on behalf of a caller who may not write.
func TestNewServer_PermissionGateAnswersBeforeTheBindingGate(t *testing.T) {
	_, g := readOnlyPrincipalCtx()
	writeTools := map[string]bool{}
	for _, r := range toolRegistrations(nil) {
		if r.write {
			writeTools[r.tool.Name] = true
		}
	}
	if len(writeTools) == 0 {
		t.Fatal("expected at least one write tool in the catalog")
	}
	// knomit_learn is gateRequired AND write: with no binding handle and no
	// write permission, the answer must be the permission one.
	h := gatePermission(g, mcpgo.Tool{Name: "knomit_learn"}, auth.Write,
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			t.Fatal("the binding gate must not be reached")
			return nil, nil
		})
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{Kind: auth.KindBridge, ID: "uid:1", Via: auth.ViaSocket})
	res, _ := h(ctx, mcpgo.CallToolRequest{})
	if !res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "permission denied") {
		t.Fatalf("res=%+v", res)
	}
}
