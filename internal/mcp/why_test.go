package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
)

func TestWhyReturnsHistory(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	factContent := SerializeFact(Fact{
		Path: "kb/foo.md", Title: "Foo", Body: "Body.",
		Domain: []string{"testing"}, Confidence: 0.9, Sources: 1,
		Entities: []string{}, Refs: []string{},
	})


	gs.EXPECT().ReadFile("kb/foo.md").Return(factContent, nil)
	gs.EXPECT().Log("kb/foo.md").Return([]LogEntry{
		{Commit: "deadbeef", Date: "2024-01-01T00:00:00Z", Message: "learn: first"},
	}, nil)
	gs.EXPECT().TagsContaining("deadbeef").Return([]string{"learn/first"}, nil)

	handler := WhyHandler(gs, "kb")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file": "kb/foo.md",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if _, ok := resp["fact"]; !ok {
		t.Fatal("missing fact in response")
	}
	if _, ok := resp["history"]; !ok {
		t.Fatal("missing history in response")
	}
	history, _ := resp["history"].([]interface{})
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}

	lm, ok := resp["learning_moment"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected learning_moment map, got: %v", resp["learning_moment"])
	}
	if lm["tag"] != "learn/first" {
		t.Fatalf("learning_moment.tag: got %q", lm["tag"])
	}
}

func TestWhyRequiresFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)



	handler := WhyHandler(gs, "kb")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}

func TestWhyFileNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	gs.EXPECT().ReadFile("kb/nonexistent.md").Return("", fmt.Errorf("not found"))

	handler := WhyHandler(gs, "kb")
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file": "kb/nonexistent.md",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for nonexistent file")
	}
}
