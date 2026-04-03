package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
)

func TestRetractDeletesFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	var deletedFile string

	gs.EXPECT().FactExists(gomock.Any(), testAgentBranch, "kb/foo.md").Return(true, nil)
	gs.EXPECT().DeleteFact(gomock.Any(), testAgentBranch, "kb/foo.md", gomock.Any()).DoAndReturn(func(ctx context.Context, branch, path, msg string) (string, error) {
		deletedFile = path
		return "abc123def456", nil
	})

	handler := RetractHandler(gs, "kb", testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/foo.md",
		"moment_name": "retract-test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify file was deleted.
	if deletedFile != "kb/foo.md" {
		t.Fatalf("expected kb/foo.md to be deleted, got: %q", deletedFile)
	}

	// Verify result JSON.
	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["file"] != "kb/foo.md" {
		t.Fatalf("response file: got %q", resp["file"])
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("missing commit in response")
	}
}

func TestRetractFileNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	gs.EXPECT().FactExists(gomock.Any(), testAgentBranch, "kb/nonexistent.md").Return(false, nil)

	handler := RetractHandler(gs, "kb", testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/nonexistent.md",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}
