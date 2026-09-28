package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// NewScriptTools is the in-process tool set a `do: script` trigger's host
// calls (F07 PR 3): the SAME handler closures the MCP server registers —
// learn, update, retract, query, explain — with the same embedder (so dedup
// and the same-subject gate are on, exactly as for a session), called with
// no transport, session or gate. The registration-time gates (binding
// resolution, permissions) wrap CLIENTS of the HTTP edge; the dispatcher is
// the server acting on its own store, and the ctx it passes already carries
// an explicit repos.Binding (the agent branch), the trailer set and the
// budget's deadline — this adapter passes that ctx through UNCHANGED, which
// is what lets an unchanged handler stamp the trailers on the commit it
// writes. It lives here, not in internal/repos, because internal/mcp imports
// internal/repos; internal/app wires it into repos.Deps.
func NewScriptTools(embedder store.BatchEmbedder) repos.ScriptTools {
	var embedders []store.BatchEmbedder
	if embedder != nil {
		embedders = append(embedders, embedder)
	}
	return scriptTools{handlers: map[string]toolHandler{
		"learn":   LearnHandler(embedders...),
		"update":  UpdateHandler(),
		"retract": RetractHandler(),
		"query":   QueryHandler(embedders...),
		"explain": ExplainHandler(),
	}}
}

type toolHandler = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

type scriptTools struct {
	handlers map[string]toolHandler
}

// Call implements repos.ScriptTools.
func (s scriptTools) Call(ctx context.Context, tool string, args map[string]any) (string, bool, error) {
	h, ok := s.handlers[tool]
	if !ok {
		return "", false, fmt.Errorf("unknown tool %q", tool)
	}
	var req mcpgo.CallToolRequest
	req.Params.Name = "knomit_" + tool
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil {
		return "", false, err
	}
	if res == nil {
		return "", false, nil
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			text = tc.Text
			break
		}
	}
	return text, res.IsError, nil
}
