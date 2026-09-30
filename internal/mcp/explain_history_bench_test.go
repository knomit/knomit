package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// BenchmarkExplainHistory measures knomit_explain's history on the
// merge-delivered fixture (8 PRs x 3 agent writes = 25 changes):
//   - FirstCall: a whole first explain call (body, graph, 3 revisions).
//   - CursorPage: one history_cursor page (up to 20 revisions, history only).
//
// Target (user requirement): single-digit milliseconds.
func BenchmarkExplainHistory(b *testing.B) {
	t := &testing.T{}
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	const path = "kb/slot.md"
	mergeDeliveredHistory(t, ctx, ri, path, 8, 3)
	if t.Failed() {
		b.Fatal("fixture setup failed")
	}

	call := func(args map[string]any) string {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		res, err := ExplainHandler()(ctx, req)
		if err != nil || res.IsError {
			b.Fatalf("explain: %v %v", err, res)
		}
		return res.Content[0].(mcpgo.TextContent).Text
	}
	var first expResp
	if err := json.Unmarshal([]byte(call(map[string]any{"file": path})), &first); err != nil {
		b.Fatal(err)
	}
	hc := first.Facts[0].History.HistoryCursor
	if hc == "" {
		b.Fatal("no history_cursor")
	}

	b.Run("FirstCall", func(b *testing.B) {
		for b.Loop() {
			call(map[string]any{"file": path})
		}
	})
	b.Run("CursorPage", func(b *testing.B) {
		for b.Loop() {
			call(map[string]any{"file": path, "history_cursor": hc})
		}
	})
}
