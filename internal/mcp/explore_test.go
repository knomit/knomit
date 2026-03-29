package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
)

func TestExploreFirstPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	ei := NewMockToolSessionIndex(ctrl)

	ts := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

	tmp := fact.NewFact("kb/foo.md")
	tmp.Title = "Foo Fact"
	tmp.Body = "Foo body."
	tmp.Domain = []string{}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	factContent := SerializeFact(tmp)

	// branch from handler arg
	ei.EXPECT().GCToolSessions("explore", "machine/test", 5).Return(nil)
	gs.EXPECT().WalkChangedFiles(testAgentBranch, "", "kb", nil, 25).Return(
		[]FileRecency{{Path: "kb/foo.md", Timestamp: ts}},
		"abc123", nil,
	)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/foo.md").Return(factContent, nil)
	ei.EXPECT().CreateToolSession("explore", "machine/test", "kb").Return(
		&ToolSession{ID: "sess-1", Tool: "explore", Branch: "machine/test", PathPrefix: "kb", Status: "active"},
		nil,
	)
	ei.EXPECT().AddSeenPaths("sess-1", []string{"kb/foo.md"}).Return(nil)
	ei.EXPECT().UpdateToolSession("sess-1", "abc123", "completed").Return(nil)

	handler := ExploreHandler(gs, ei, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

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

	facts, ok := resp["facts"].([]interface{})
	if !ok || len(facts) != 1 {
		t.Fatalf("expected 1 fact, got: %v", resp["facts"])
	}
	f := facts[0].(map[string]interface{})
	if f["path"] != "kb/foo.md" {
		t.Fatalf("expected path kb/foo.md, got %v", f["path"])
	}
	if f["title"] != "Foo Fact" {
		t.Fatalf("expected title Foo Fact, got %v", f["title"])
	}
	if resp["has_more"] != false {
		t.Fatalf("expected has_more=false, got %v", resp["has_more"])
	}
}

func TestExploreResumesSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	ei := NewMockToolSessionIndex(ctrl)

	ts := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

	tmp := fact.NewFact("kb/bar.md")
	tmp.Title = "Bar Fact"
	tmp.Body = "Bar body."
	tmp.Domain = []string{}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	factContent := SerializeFact(tmp)

	seen := map[string]bool{"kb/foo.md": true}

	// branch from handler arg
	ei.EXPECT().GetToolSession("sess-1").Return(
		&ToolSession{ID: "sess-1", Tool: "explore", Branch: "machine/test", PathPrefix: "kb", LastCommit: "abc123", Status: "active"},
		nil,
	)
	ei.EXPECT().GetSeenPaths("sess-1").Return(seen, nil)
	gs.EXPECT().WalkChangedFiles(testAgentBranch, "abc123", "kb", seen, 25).Return(
		[]FileRecency{{Path: "kb/bar.md", Timestamp: ts}},
		"def456", nil,
	)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/bar.md").Return(factContent, nil)
	ei.EXPECT().AddSeenPaths("sess-1", []string{"kb/bar.md"}).Return(nil)
	ei.EXPECT().UpdateToolSession("sess-1", "def456", "completed").Return(nil)

	handler := ExploreHandler(gs, ei, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"cursor": "sess-1",
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

	facts := resp["facts"].([]interface{})
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if facts[0].(map[string]interface{})["title"] != "Bar Fact" {
		t.Fatalf("expected Bar Fact, got %v", facts[0])
	}
}

func TestExploreEmptyKB(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	ei := NewMockToolSessionIndex(ctrl)

	// branch from handler arg
	ei.EXPECT().GCToolSessions("explore", "machine/test", 5).Return(nil)
	gs.EXPECT().WalkChangedFiles(testAgentBranch, "", "kb", nil, 25).Return(nil, "", nil)

	handler := ExploreHandler(gs, ei, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

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

	facts := resp["facts"].([]interface{})
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts, got %d", len(facts))
	}
	if resp["cursor"] != nil {
		t.Fatalf("expected nil cursor, got %v", resp["cursor"])
	}
	if resp["has_more"] != false {
		t.Fatalf("expected has_more=false, got %v", resp["has_more"])
	}
}

func TestExploreExpiredSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	ei := NewMockToolSessionIndex(ctrl)

	// branch from handler arg
	ei.EXPECT().GetToolSession("gone-sess").Return(nil, nil)

	handler := ExploreHandler(gs, ei, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"cursor": "gone-sess",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for expired session")
	}
}

func TestExploreDeletedFactSkipped(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	ei := NewMockToolSessionIndex(ctrl)

	ts := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

	tmp := fact.NewFact("kb/good.md")
	tmp.Title = "Good Fact"
	tmp.Body = "Good body."
	tmp.Domain = []string{}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	goodContent := SerializeFact(tmp)

	// branch from handler arg
	ei.EXPECT().GCToolSessions("explore", "machine/test", 5).Return(nil)
	gs.EXPECT().WalkChangedFiles(testAgentBranch, "", "kb", nil, 25).Return(
		[]FileRecency{
			{Path: "kb/deleted.md", Timestamp: ts},
			{Path: "kb/good.md", Timestamp: ts},
		},
		"abc123", nil,
	)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/deleted.md").Return("", fmt.Errorf("not found"))
	gs.EXPECT().ReadFile(testAgentBranch, "kb/good.md").Return(goodContent, nil)
	ei.EXPECT().CreateToolSession("explore", "machine/test", "kb").Return(
		&ToolSession{ID: "sess-2", Tool: "explore", Branch: "machine/test", PathPrefix: "kb", Status: "active"},
		nil,
	)
	ei.EXPECT().AddSeenPaths("sess-2", []string{"kb/good.md"}).Return(nil)
	ei.EXPECT().UpdateToolSession("sess-2", "abc123", "completed").Return(nil)

	handler := ExploreHandler(gs, ei, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

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

	facts := resp["facts"].([]interface{})
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact (deleted skipped), got %d", len(facts))
	}
	if facts[0].(map[string]interface{})["title"] != "Good Fact" {
		t.Fatalf("expected Good Fact, got %v", facts[0])
	}
}
