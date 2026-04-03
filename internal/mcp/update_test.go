package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
)

func TestUpdateMergesFields(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	tmp := fact.NewFact("kb/foo.md")
	tmp.Title = "Original Title"
	tmp.Body = "Original body."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.7
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"https://old.ref"}
	factContent := SerializeFact(tmp)

	var writtenContent string

	gs.EXPECT().FactExists(gomock.Any(), testAgentBranch, "kb/foo.md").Return(true, nil)
	gs.EXPECT().ReadFact(gomock.Any(), testAgentBranch, "kb/foo.md", gomock.Any()).Return(ReadFactResult{Content: factContent}, nil)
	gs.EXPECT().WriteFact(gomock.Any(), testAgentBranch, "kb/foo.md", gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, branch, path, content, msg, operation string) (WriteFactResult, error) {
		writtenContent = content
		return WriteFactResult{CommitHash: "abc123def456", BlobHash: "blob_foo"}, nil
	})

	handler := UpdateHandler(gs, "kb", testAgentBranch)

	newBody := "Updated body."
	newConf := 0.95

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/foo.md",
		"moment_name": "update-test",
		"updates": map[string]interface{}{
			"body":       newBody,
			"confidence": newConf,
			"refs":       []interface{}{"https://new.ref"},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify file was updated.
	if writtenContent == "" {
		t.Fatal("expected kb/foo.md to be written")
	}

	updatedFact, err := ParseFact("kb/foo.md", writtenContent)
	if err != nil {
		t.Fatalf("parse updated fact: %v", err)
	}
	if updatedFact.Body != newBody {
		t.Fatalf("body: got %q want %q", updatedFact.Body, newBody)
	}
	if updatedFact.Confidence != newConf {
		t.Fatalf("confidence: got %v want %v", updatedFact.Confidence, newConf)
	}
	// Refs should be appended, not replaced.
	if len(updatedFact.Refs) != 2 {
		t.Fatalf("refs: expected 2, got %v", updatedFact.Refs)
	}

	// Verify result JSON.
	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := resp["commit"]; !ok {
		t.Fatal("missing commit in response")
	}
}

func TestUpdateFileNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().FactExists(gomock.Any(), testAgentBranch, "kb/nonexistent.md").Return(false, nil)

	handler := UpdateHandler(gs, "kb", testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/nonexistent.md",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}

func TestUpdateRefsAppended(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	tmp := fact.NewFact("kb/refs.md")
	tmp.Title = "Refs Test"
	tmp.Body = "Body."
	tmp.Domain = []string{}
	tmp.Confidence = 0.8
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"https://existing.ref"}
	factContent := SerializeFact(tmp)

	var writtenContent string

	gs.EXPECT().FactExists(gomock.Any(), testAgentBranch, "kb/refs.md").Return(true, nil)
	gs.EXPECT().ReadFact(gomock.Any(), testAgentBranch, "kb/refs.md", gomock.Any()).Return(ReadFactResult{Content: factContent}, nil)
	gs.EXPECT().WriteFact(gomock.Any(), testAgentBranch, "kb/refs.md", gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, branch, path, content, msg, operation string) (WriteFactResult, error) {
		writtenContent = content
		return WriteFactResult{CommitHash: "abc123def456", BlobHash: "blob_refs"}, nil
	})

	handler := UpdateHandler(gs, "kb", testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/refs.md",
		"moment_name": "refs-append",
		"updates": map[string]interface{}{
			"refs": []interface{}{"https://new1.ref", "https://new2.ref"},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	updatedFact, err := ParseFact("kb/refs.md", writtenContent)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updatedFact.Refs) != 3 {
		t.Fatalf("expected 3 refs, got %v", updatedFact.Refs)
	}
}
