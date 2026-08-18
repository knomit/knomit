package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// retractTool returns the Tool definition for knomit_retract.
func retractTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_retract",
		mcpgo.WithDescription("Retract a fact from the knowledge base."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file to retract."),
		),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this retraction moment."),
		),
	)
}

// RetractHandler returns the handler function for knomit_retract.
func RetractHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
		}
		ri := b.Write()
		s, release, err := storeIndices(ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer release()
		agentBranch := ri.AgentBranch()
		ontologyRoot := ri.OntologyRoot()

		// 1. Get arguments.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file, err = federate.WriteRepoPath(b, file)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		file = fact.NormalizePath(ontologyRoot, file)
		// A DELETE is a write. Without this, retract would happily remove a
		// hand-placed kb/.drafts/ file that create and update both refuse to
		// write — an asymmetry with no rationale behind it.
		if fact.IsPrivatePath(file) && !fact.IsWritablePrivatePath(file) {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"%s is private: a path segment beginning with '.' cannot hold a fact, "+
					"except under %s/<area>/", file, fact.PrivateRoot)), nil
		}
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// 3. Check file exists.
		exists, err := s.facts.FactExists(ctx, agentBranch, file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("file exists check error: %v", err)), nil
		}
		if !exists {
			return mcpgo.NewToolResultError(fmt.Sprintf("file not found: %s", file)), nil
		}

		// 4. Delete the file.
		commitMsg := fmt.Sprintf("retract(%s): %s", momentName, file)
		hash, err := s.facts.DeleteFact(ctx, agentBranch, file, commitMsg)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("delete error: %v", err)), nil
		}

		dest := describeWriteDestination(b)
		result := map[string]interface{}{
			"file":       file,
			"commit":     hash,
			"written_to": dest,
			"summary":    dest.summary("1 retraction"),
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
