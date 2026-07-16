package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/repos"
)

// reposTool defines knomit_repos: the binding discovery tool.
func reposTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_repos",
		mcpgo.WithDescription("List the repos (mounts) behind this endpoint: name, stable repo id (root commit hash), branch, role (read or read+write), and optional src:// source slug. Use the id to interpret kb://<id>/… paths and refs."),
	)
}

// reposMount is one mount row in the knomit_repos response.
type reposMount struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Role   string `json:"role"`
	Source string `json:"source,omitempty"`
}

// reposResponse is the knomit_repos envelope.
type reposResponse struct {
	Binding string       `json:"binding"`
	Mounts  []reposMount `json:"mounts"`
}

// ReposHandler returns the handler for knomit_repos. Read-only: it reports
// the binding's mounts and never touches a store.
func ReposHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		b := repos.BindingFromContext(ctx)
		resp := reposResponse{Binding: b.Name()}
		for _, rt := range b.Reads() {
			role := "read"
			if rt.RI == b.Write() {
				role = "read+write"
			}
			resp.Mounts = append(resp.Mounts, reposMount{
				Name:   rt.RI.Name(),
				ID:     rt.RI.ID(),
				Branch: rt.Branch,
				Role:   role,
				Source: rt.Source,
			})
		}
		out, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultError("marshal error: " + err.Error()), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
