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

// LoadPrefix initializes c as info and pages tools/list (following
// nextCursor until the server returns none), returning the
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
	var tools []llm.ToolDef
	seen := map[mcpgo.Cursor]bool{}
	var req mcpgo.ListToolsRequest
	for {
		// One page per call: Client.ListTools (mcp-go v0.45.0) would follow
		// the cursor itself, with no guard against a cursor that repeats.
		list, err := c.ListToolsByPage(ctx, req)
		if err != nil {
			return "", nil, fmt.Errorf("chat prefix: tools/list: %w", err)
		}
		for _, t := range list.Tools {
			def, err := toolDef(t)
			if err != nil {
				return "", nil, err
			}
			tools = append(tools, def)
		}
		if list.NextCursor == "" {
			break
		}
		if seen[list.NextCursor] {
			return "", nil, fmt.Errorf("chat prefix: tools/list: cursor %q repeated", list.NextCursor)
		}
		seen[list.NextCursor] = true
		req.Params.Cursor = list.NextCursor
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

// NewChatToolWindow builds the ToolWindow of an adaptive chat from the tools
// the principal was shown (the same tools AddPrefix puts in hook 1, which stay
// in the Builder: the loop sends only the window's projection of them).
//
// The loader's description carries the catalogue, not the system prompt, so
// hook 2 is byte-identical for every provider and for static and adaptive
// chats alike. budget is the catalogue's allowance in tokens (the preload
// budget P; 0 or less: no limit); a *CatalogueTooBigError means even the tool
// names do not fit it. mcp.Replayable tells the window which results a stub may
// offer back; ranker may be nil (no preload).
func NewChatToolWindow(tools []llm.ToolDef, ranker llm.ToolRanker, budget int) (*llm.ToolWindow, error) {
	cat, err := BuildCatalogue(tools, budget)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return llm.NewToolWindow(llm.ToolWindowSpec{
		Loader: llm.LoaderDef(cat), Names: names, Replayable: Replayable, Ranker: ranker,
		Core: ChatCoreTools,
	})
}

// ChatCoreTools is the policy list of tools an adaptive chat always has in its
// window: loaded from the opening, never evicted, never a preload pick. They
// are the two nearly every turn needs: knomit_bind (nothing else works until a
// repo is bound) and knomit_query (the plain "what do we know about..."
// question). Chosen because the live acceptance run showed a 4B model does not
// call load_tools, so a tool it must load first is a tool it never uses; every
// other tool stays adaptive (preload plus load_tools). The list is policy, not
// a measurement of usage, and it adds no more than these two definitions to
// the opening (TestChatToolWindow_OpeningWithTheCoreSetFitsA32KModel).
var ChatCoreTools = []string{"knomit_bind", "knomit_query"}

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
