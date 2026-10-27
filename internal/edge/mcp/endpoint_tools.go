package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// endpointToolFilter serves each endpoint kind its own tool schemas.
//
// The tools are registered once, with the UNSCOPED endpoint's texts. A
// URL-scoped endpoint (a --repo or --lens bridge) differs in two places, and an
// agent reading the unscoped texts there would act on false statements:
//
//   - every `binding` is optional and its absence means "this connection's own
//     repo or lens" (bindingDescription's scoped texts), and
//   - knomit_bind takes no repo or lens, only the branch selector
//     (scopedBindTool).
//
// Presentation only, like permissionFilter: what a call may do is decided by
// gateBinding and BindHandler on the registration seam, which read the same
// endpoint marker this does.
func endpointToolFilter(gates map[string]gateMode) mcpserver.ToolFilterFunc {
	return func(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
		kind := endpointKindOf(ctx)
		if kind == endpointUnscoped {
			return tools
		}
		return toolsForEndpoint(tools, gates, kind)
	}
}

// toolsForEndpoint rewrites a tool list for a URL-scoped endpoint of the given
// kind. It never mutates its input: the registered tools are shared by every
// session.
//
// On an endpointFixed URL knomit_bind is NOT LISTED: every call to it there is
// refused, and advertising a tool that always fails is the shape of the loop
// that started this work.
func toolsForEndpoint(tools []mcpgo.Tool, gates map[string]gateMode, kind endpointKind) []mcpgo.Tool {
	out := make([]mcpgo.Tool, 0, len(tools))
	for _, t := range tools {
		mode, known := gates[t.Name]
		switch {
		case !known:
			out = append(out, t)
		case mode == gateUngated && kind == endpointFixed:
			// knomit_bind is the only ungated tool; nothing can bind here.
		case mode == gateUngated:
			out = append(out, scopedBindTool())
		default:
			out = append(out, withBindingDescription(t, bindingDescription(mode, kind)))
		}
	}
	return out
}

// withBindingDescription returns t with its `binding` property's description
// replaced, copying the property maps it touches.
func withBindingDescription(t mcpgo.Tool, desc string) mcpgo.Tool {
	prop, ok := t.InputSchema.Properties[bindingArgument].(map[string]any)
	if !ok {
		return t
	}
	np := make(map[string]any, len(prop))
	for k, v := range prop {
		np[k] = v
	}
	np["description"] = desc
	props := make(map[string]any, len(t.InputSchema.Properties))
	for k, v := range t.InputSchema.Properties {
		props[k] = v
	}
	props[bindingArgument] = np
	t.InputSchema.Properties = props
	return t
}

// gateModes maps each registered tool to its gate mode.
func gateModes(regs []toolReg) map[string]gateMode {
	out := make(map[string]gateMode, len(regs))
	for _, r := range regs {
		out[r.tool.Name] = r.gate
	}
	return out
}
