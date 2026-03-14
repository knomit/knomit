package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
)

func TestForgetDeletesFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var deletedFile string
	var deletedFromIndex string
	var tagSet string

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/foo.md").Return(true, nil)
	gs.EXPECT().DeleteFile("know/foo.md", gomock.Any()).DoAndReturn(func(path, msg string) (string, error) {
		deletedFile = path
		return "abc123def456", nil
	})
	idx.EXPECT().Delete("know/foo.md").DoAndReturn(func(path string) error {
		deletedFromIndex = path
		return nil
	})
	gs.EXPECT().Tag(gomock.Any()).DoAndReturn(func(name string) error {
		tagSet = name
		return nil
	})

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
		"moment_name": "forget-test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify file was deleted.
	if deletedFile != "know/foo.md" {
		t.Fatalf("expected know/foo.md to be deleted, got: %q", deletedFile)
	}

	// Verify index delete was called.
	if deletedFromIndex != "know/foo.md" {
		t.Fatalf("expected index delete for know/foo.md, got: %q", deletedFromIndex)
	}

	// Verify tag was set.
	if tagSet == "" {
		t.Fatal("expected forget tag to be set")
	}
	if !strings.HasPrefix(tagSet, "forget/") {
		t.Fatalf("tag should start with forget/, got %q", tagSet)
	}

	// Verify result JSON.
	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["file"] != "know/foo.md" {
		t.Fatalf("response file: got %q", resp["file"])
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("missing commit in response")
	}
}

func TestForgetFileNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/nonexistent.md").Return(false, nil)

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/nonexistent.md",
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
