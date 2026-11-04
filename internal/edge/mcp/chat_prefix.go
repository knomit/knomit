package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/engine/llm"
)

// Builder item ids of the shared prefix.
const (
	PrefixSystemItem       = "system"
	PrefixInstructionsItem = "instructions"
	prefixToolItemPrefix   = "tool:"
)

// LoadPrefix initializes c as info and pages tools/list, returning the
// initialize instructions (hook 3) and the tool definitions (hook 1), sorted
// by name with canonical schemas.
//
// Both come from the server exactly as any MCP client receives them: the
// instructions are what the AfterInitialize hook computed for the context's
// endpoint kind, and the tools are what the permission and endpoint filters
// expose to the context's principal. That is why hook 1 is shared per
// permission set and hook 3 per endpoint kind, and why nothing here filters
// or rewrites either: a machine-specific byte found in them must be fixed in
// the server, where external agents see the same text.
func LoadPrefix(ctx context.Context, c *client.Client, info mcpgo.Implementation) (string, []llm.ToolDef, error) {
	if err := c.Start(ctx); err != nil {
		return "", nil, fmt.Errorf("chat prefix: start: %w", err)
	}
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = info
	res, err := c.Initialize(ctx, init)
	if err != nil {
		return "", nil, fmt.Errorf("chat prefix: initialize: %w", err)
	}
	list, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		return "", nil, fmt.Errorf("chat prefix: tools/list: %w", err)
	}
	tools := make([]llm.ToolDef, 0, len(list.Tools))
	for _, t := range list.Tools {
		def, err := toolDef(t)
		if err != nil {
			return "", nil, err
		}
		tools = append(tools, def)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return res.Instructions, tools, nil
}

// toolDef takes the schema as the tool marshals on the wire (inputSchema,
// whichever of the raw or structured forms the tool carries) and
// canonicalizes it, so map order inside the schema never reaches the bytes.
func toolDef(t mcpgo.Tool) (llm.ToolDef, error) {
	wire, err := json.Marshal(t)
	if err != nil {
		return llm.ToolDef{}, fmt.Errorf("chat prefix: marshal %s: %w", t.Name, err)
	}
	var w struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(wire, &w); err != nil {
		return llm.ToolDef{}, fmt.Errorf("chat prefix: %s: %w", t.Name, err)
	}
	schema, err := llm.CanonicalJSON(w.InputSchema)
	if err != nil {
		return llm.ToolDef{}, fmt.Errorf("chat prefix: %s schema: %w", t.Name, err)
	}
	return llm.ToolDef{Name: t.Name, Description: t.Description, Schema: schema}, nil
}

// AddPrefix puts the shared hooks into b: one tool:<name> item per tool
// (hook 1), ChatSystemPrompt as "system" (hook 2) and instructions as
// "instructions" (hook 3).
func AddPrefix(b *llm.Builder, instructions string, tools []llm.ToolDef) error {
	for i := range tools {
		t := tools[i]
		if err := b.Add(llm.Item{ID: prefixToolItemPrefix + t.Name, Hook: llm.HookTools, Tool: &t}); err != nil {
			return err
		}
	}
	if err := b.Add(llm.Item{ID: PrefixSystemItem, Hook: llm.HookSystem, Text: ChatSystemPrompt}); err != nil {
		return err
	}
	return b.Add(llm.Item{ID: PrefixInstructionsItem, Hook: llm.HookRepo, Text: instructions})
}
