package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// exploreTool returns the Tool definition for knomit_explore.
func exploreTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_explore",
		mcpgo.WithDescription("List the contents of a knowledge base path."),
		mcpgo.WithString("path",
			mcpgo.Description("Path to explore (default: \"know\")."),
		),
	)
}

// ExploreHandler returns the handler function for knomit_explore.
func ExploreHandler(gs GitStore) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// 1. Sync.
		if _, err := gs.Sync(nil); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("sync error: %v", err)), nil
		}

		// 2. Get path parameter (default "know").
		path := req.GetString("path", "know")

		// 3. Read manifest: <path>.md if it exists.
		var manifest interface{}
		manifestPath := path + ".md"
		manifestContent, err := gs.ReadFile(manifestPath)
		if err == nil {
			if f, parseErr := ParseFact(manifestPath, manifestContent); parseErr == nil {
				manifest = map[string]interface{}{
					"title": f.Title,
					"body":  f.Body,
				}
			}
		}

		// 4. List directory.
		entries, err := gs.ListDir(path)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("list dir error: %v", err)), nil
		}

		type childOutput struct {
			Name    string `json:"name"`
			IsDir   bool   `json:"is_dir"`
			Summary string `json:"summary,omitempty"`
		}

		children := make([]childOutput, 0, len(entries))
		for _, e := range entries {
			child := childOutput{
				Name:  e.Name,
				IsDir: e.IsDir,
			}
			// Try to read summary from manifest or file.
			if e.IsDir {
				// Directory: try to read <path>/<name>.md as manifest.
				subManifestPath := path + "/" + e.Name + ".md"
				if content, readErr := gs.ReadFile(subManifestPath); readErr == nil {
					if f, parseErr := ParseFact(subManifestPath, content); parseErr == nil {
						child.Summary = f.Title
					}
				}
			} else {
				// File: strip ".md" suffix for name display, try to parse for title.
				name := strings.TrimSuffix(e.Name, ".md")
				child.Name = name
				filePath := path + "/" + e.Name
				if content, readErr := gs.ReadFile(filePath); readErr == nil {
					if f, parseErr := ParseFact(filePath, content); parseErr == nil {
						child.Summary = f.Title
					}
				}
			}
			children = append(children, child)
		}

		// 5. Walk parent dirs collecting inherited facts.
		var inheritedFacts []map[string]interface{}
		inheritedFacts = collectInheritedFacts(gs, path)

		result := map[string]interface{}{
			"manifest":        manifest,
			"children":        children,
			"inherited_facts": inheritedFacts,
		}

		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// collectInheritedFacts walks up the directory tree from path, collecting
// facts found at each parent level. Returns a flat list of fact summaries.
func collectInheritedFacts(gs GitStore, path string) []map[string]interface{} {
	var inherited []map[string]interface{}

	// Walk up: e.g. "know/a/b" → "know/a" → "know" → stop at root.
	current := path
	for {
		// Find last slash.
		idx := strings.LastIndex(current, "/")
		if idx < 0 {
			break
		}
		current = current[:idx]
		if current == "" {
			break
		}

		// List entries at this level.
		entries, err := gs.ListDir(current)
		if err != nil {
			break
		}
		for _, e := range entries {
			if e.IsDir {
				continue // Skip subdirectories.
			}
			filePath := current + "/" + e.Name
			content, err := gs.ReadFile(filePath)
			if err != nil {
				continue
			}
			f, err := ParseFact(filePath, content)
			if err != nil {
				continue
			}
			inherited = append(inherited, map[string]interface{}{
				"file":  filePath,
				"title": f.Title,
			})
		}
	}
	return inherited
}
