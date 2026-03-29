package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
)

func TestExplainFirstPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/root.md")
	tmp.Title = "Root"
	tmp.Body = "Root body."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"kb/tech/ref1.md", "https://example.com"}
	factContent := SerializeFact(tmp)

	// branch from handler arg
	sessionIdx.EXPECT().GCToolSessions("explain", "machine/test", 5).Return(nil)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/root.md").Return(factContent, nil)
	gs.EXPECT().Log(testAgentBranch, "kb/root.md").Return([]LogEntry{
		{Commit: "abc12345", Date: "2026-03-14T10:00:00Z", Message: "learn: root"},
	}, nil)
	sessionIdx.EXPECT().CreateToolSession("explain", "machine/test", "kb/root.md").Return(&ToolSession{ID: "sess-1", Status: "active"}, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-1", []string{"kb/root.md"}).Return(nil)
	sessionIdx.EXPECT().EnqueuePaths("sess-1", []QueueItem{
		{Path: "kb/tech/ref1.md", CommitHash: "abc12345", Depth: 1},
	}).Return(nil)
	sessionIdx.EXPECT().QueueSize("sess-1").Return(1, nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file": "kb/root.md",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		Cursor  *string            `json:"cursor"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	f := resp.Facts[0]
	if f.Depth != 0 {
		t.Fatalf("expected depth 0, got %d", f.Depth)
	}
	if len(f.Refs.Local) != 1 || f.Refs.Local[0] != "kb/tech/ref1.md" {
		t.Fatalf("unexpected local refs: %v", f.Refs.Local)
	}
	if len(f.Refs.External) != 1 || f.Refs.External[0] != "https://example.com" {
		t.Fatalf("unexpected external refs: %v", f.Refs.External)
	}
	if !resp.HasMore {
		t.Fatal("expected has_more=true")
	}
	if resp.Cursor == nil || *resp.Cursor != "sess-1" {
		t.Fatalf("expected cursor=sess-1, got %v", resp.Cursor)
	}
}

func TestExplainResumesSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/ref1.md")
	tmp.Title = "Ref1"
	tmp.Body = "Ref body."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.8
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"kb/deep.md"}
	refContent := SerializeFact(tmp)

	sessionIdx.EXPECT().GetToolSession("sess-1").Return(&ToolSession{ID: "sess-1", Status: "active"}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-1").Return(map[string]bool{"kb/root.md": true}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-1", 25).Return([]QueueItem{
		{Path: "kb/ref1.md", CommitHash: "abc123", Depth: 1},
	}, nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/ref1.md", "abc123").Return(refContent, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-1", []string{"kb/ref1.md"}).Return(nil)
	sessionIdx.EXPECT().EnqueuePaths("sess-1", []QueueItem{
		{Path: "kb/deep.md", CommitHash: "abc123", Depth: 2},
	}).Return(nil)
	sessionIdx.EXPECT().QueueSize("sess-1").Return(1, nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-1",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		Cursor  *string            `json:"cursor"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	if resp.Facts[0].Depth != 1 {
		t.Fatalf("expected depth 1, got %d", resp.Facts[0].Depth)
	}
	if !resp.HasMore {
		t.Fatal("expected has_more=true")
	}
}

func TestExplainNoRefs(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/solo.md")
	tmp.Title = "Solo"
	tmp.Body = "No refs."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	factContent := SerializeFact(tmp)

	// branch from handler arg
	sessionIdx.EXPECT().GCToolSessions("explain", "machine/test", 5).Return(nil)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/solo.md").Return(factContent, nil)
	gs.EXPECT().Log(testAgentBranch, "kb/solo.md").Return([]LogEntry{
		{Commit: "def456", Date: "2026-03-14T10:00:00Z", Message: "learn: solo"},
	}, nil)
	sessionIdx.EXPECT().CreateToolSession("explain", "machine/test", "kb/solo.md").Return(&ToolSession{ID: "sess-2", Status: "active"}, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-2", []string{"kb/solo.md"}).Return(nil)
	// No EnqueuePaths call — no local refs.
	sessionIdx.EXPECT().QueueSize("sess-2").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-2", "def456", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file": "kb/solo.md",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		Cursor  *string            `json:"cursor"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if resp.HasMore {
		t.Fatal("expected has_more=false")
	}
	if resp.Cursor != nil {
		t.Fatalf("expected cursor=null, got %v", resp.Cursor)
	}
}

func TestExplainExpiredSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	sessionIdx.EXPECT().GetToolSession("expired-1").Return(nil, nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "expired-1",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for expired session")
	}
}

func TestExplainDeletedRef(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/good.md")
	tmp.Title = "Good"
	tmp.Body = "Still here."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	goodContent := SerializeFact(tmp)

	sessionIdx.EXPECT().GetToolSession("sess-3").Return(&ToolSession{ID: "sess-3", Status: "active"}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-3").Return(map[string]bool{"kb/root.md": true}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-3", 25).Return([]QueueItem{
		{Path: "kb/deleted.md", CommitHash: "abc123", Depth: 1},
		{Path: "kb/good.md", CommitHash: "abc123", Depth: 1},
	}, nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/deleted.md", "abc123").Return("", fmt.Errorf("not found"))
	gs.EXPECT().LastCommitForPath(testAgentBranch, "kb/deleted.md").Return("", nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/good.md", "abc123").Return(goodContent, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-3", []string{"kb/good.md"}).Return(nil)
	// No new local refs to enqueue.
	sessionIdx.EXPECT().QueueSize("sess-3").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-3", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-3",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact (skipping deleted), got %d", len(resp.Facts))
	}
	if resp.Facts[0].Path != "kb/good.md" {
		t.Fatalf("expected kb/good.md, got %s", resp.Facts[0].Path)
	}
}

func TestExplainMaxDepth(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/deep.md")
	tmp.Title = "Deep"
	tmp.Body = "Very deep."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"kb/deeper.md"}
	deepContent := SerializeFact(tmp)

	sessionIdx.EXPECT().GetToolSession("sess-4").Return(&ToolSession{ID: "sess-4", Status: "active"}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-4").Return(map[string]bool{"kb/root.md": true}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-4", 25).Return([]QueueItem{
		{Path: "kb/deep.md", CommitHash: "abc123", Depth: 10},
	}, nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/deep.md", "abc123").Return(deepContent, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-4", []string{"kb/deep.md"}).Return(nil)
	// EnqueuePaths should NOT be called — depth is at max.
	sessionIdx.EXPECT().QueueSize("sess-4").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-4", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-4",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	// Refs should still be reported even at max depth.
	if len(resp.Facts[0].Refs.Local) != 1 || resp.Facts[0].Refs.Local[0] != "kb/deeper.md" {
		t.Fatalf("expected local ref kb/deeper.md, got %v", resp.Facts[0].Refs.Local)
	}
}

func TestExplainExternalRefsOnly(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/ext.md")
	tmp.Title = "External"
	tmp.Body = "Only external refs."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.9
	tmp.Sources = 1
	tmp.Entities = []string{}
	tmp.Refs = []string{"https://example.com", "https://go.dev"}
	factContent := SerializeFact(tmp)

	// branch from handler arg
	sessionIdx.EXPECT().GCToolSessions("explain", "machine/test", 5).Return(nil)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/ext.md").Return(factContent, nil)
	gs.EXPECT().Log(testAgentBranch, "kb/ext.md").Return([]LogEntry{
		{Commit: "ext123", Date: "2026-03-14T10:00:00Z", Message: "learn: external"},
	}, nil)
	sessionIdx.EXPECT().CreateToolSession("explain", "machine/test", "kb/ext.md").Return(&ToolSession{ID: "sess-5", Status: "active"}, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-5", []string{"kb/ext.md"}).Return(nil)
	// No EnqueuePaths — no local refs.
	sessionIdx.EXPECT().QueueSize("sess-5").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-5", "ext123", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file": "kb/ext.md",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		Cursor  *string            `json:"cursor"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}

	if resp.HasMore {
		t.Fatal("expected has_more=false")
	}
	if resp.Cursor != nil {
		t.Fatalf("expected cursor=null, got %v", resp.Cursor)
	}
	if len(resp.Facts[0].Refs.External) != 2 {
		t.Fatalf("expected 2 external refs, got %d", len(resp.Facts[0].Refs.External))
	}
}

func TestExplainMissingFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	// branch from handler arg
	sessionIdx.EXPECT().GCToolSessions("explain", "machine/test", 5).Return(nil)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/gone.md").Return("", fmt.Errorf("not found"))

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"file": "kb/gone.md"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file")
	}
}

func TestExplainRequiresFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing file param")
	}
}

func TestExplainResumeEmptyQueue(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	sessionIdx.EXPECT().GetToolSession("sess-done").Return(&ToolSession{
		ID: "sess-done", Tool: "explain", Status: "active",
	}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-done").Return(map[string]bool{"kb/root.md": true}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-done", 25).Return(nil, nil)
	sessionIdx.EXPECT().QueueSize("sess-done").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-done", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-done",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	if resp.HasMore {
		t.Fatal("expected has_more=false for empty queue")
	}
}

func TestExplainRetractedRef(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/retracted.md")
	tmp.Title = "Retracted"
	tmp.Body = "Was here."
	tmp.Domain = []string{"testing"}
	tmp.Confidence = 0.8
	tmp.Sources = 3
	tmp.Entities = []string{}
	tmp.Refs = []string{}
	retractedContent := SerializeFact(tmp)

	sessionIdx.EXPECT().GetToolSession("sess-ret").Return(&ToolSession{ID: "sess-ret", Status: "active"}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-ret").Return(map[string]bool{"kb/root.md": true}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-ret", 25).Return([]QueueItem{
		{Path: "kb/retracted.md", CommitHash: "merge-commit", Depth: 1},
	}, nil)
	// ReadFileAtCommit fails — file was retracted before merge-commit.
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/retracted.md", "merge-commit").Return("", fmt.Errorf("not found"))
	// Fallback: find the retraction commit, then read from just before it.
	gs.EXPECT().LastCommitForPath(testAgentBranch, "kb/retracted.md").Return("retract-commit", nil)
	gs.EXPECT().ReadFileLastCommit(testAgentBranch, "kb/retracted.md", "retract-commit").Return(retractedContent, "last-live-commit", nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-ret", []string{"kb/retracted.md"}).Return(nil)
	sessionIdx.EXPECT().QueueSize("sess-ret").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-ret", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-ret",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 retracted fact, got %d", len(resp.Facts))
	}
	f := resp.Facts[0]
	if !f.Retracted {
		t.Error("expected Retracted=true")
	}
	if f.LastCommitHash != "last-live-commit" {
		t.Errorf("LastCommitHash: got %q, want %q", f.LastCommitHash, "last-live-commit")
	}
	if f.Title != "Retracted" {
		t.Errorf("Title: got %q, want %q", f.Title, "Retracted")
	}
	if f.Body == "" {
		t.Error("expected non-empty Body for retracted fact")
	}
}

func TestExplainResumeParseError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	sessionIdx.EXPECT().GetToolSession("sess-pe").Return(&ToolSession{
		ID: "sess-pe", Tool: "explain", Status: "active",
	}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-pe").Return(map[string]bool{"kb/root.md": true}, nil)
	// First dequeue: one item, ReadFileAtCommit returns invalid content.
	sessionIdx.EXPECT().DequeuePaths("sess-pe", 25).Return([]QueueItem{
		{Path: "kb/bad.md", CommitHash: "abc123", Depth: 1},
	}, nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/bad.md", "abc123").Return("not valid frontmatter", nil)
	// Retry dequeue: empty, stop.
	sessionIdx.EXPECT().DequeuePaths("sess-pe", 25).Return(nil, nil)
	sessionIdx.EXPECT().QueueSize("sess-pe").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-pe", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/root.md",
		"cursor": "sess-pe",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", getResultText(t, result))
	}

	text := getResultText(t, result)
	var resp struct {
		Facts   []explainFactEntry `json:"facts"`
		HasMore bool               `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	if resp.HasMore {
		t.Fatal("expected has_more=false")
	}
}

func TestExplainFirstPageIncludesAllFactFields(t *testing.T) {
	// Regression: explainFactEntry was missing domain, confidence, sources,
	// entities, and evidence_weight. All fields must be present in the response.
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/full.md")
	tmp.Title = "Full Fact"
	tmp.Body = "Full body."
	tmp.Type = fact.EpistemicType("observation")
	tmp.Domain = []string{"engineering", "testing"}
	tmp.Confidence = 0.85
	tmp.Sources = 3
	tmp.Entities = []string{"Go", "knomit"}
	tmp.Refs = []string{}
	tmp.EvidenceWeight = 0.7
	factContent := SerializeFact(tmp)

	// branch from handler arg
	sessionIdx.EXPECT().GCToolSessions("explain", "machine/test", 5).Return(nil)
	gs.EXPECT().ReadFile(testAgentBranch, "kb/full.md").Return(factContent, nil)
	gs.EXPECT().Log(testAgentBranch, "kb/full.md").Return([]LogEntry{
		{Commit: "deadbeef", Date: "2026-03-22T10:00:00Z", Message: "learn: full"},
	}, nil)
	sessionIdx.EXPECT().CreateToolSession("explain", "machine/test", "kb/full.md").Return(&ToolSession{ID: "sess-full", Status: "active"}, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-full", []string{"kb/full.md"}).Return(nil)
	sessionIdx.EXPECT().QueueSize("sess-full").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-full", "deadbeef", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"file": "kb/full.md"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	var resp struct {
		Facts []explainFactEntry `json:"facts"`
	}
	if err := json.Unmarshal([]byte(getResultText(t, result)), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	f := resp.Facts[0]

	if f.Confidence != 0.85 {
		t.Errorf("confidence: got %v, want 0.85", f.Confidence)
	}
	if f.Sources != 3 {
		t.Errorf("sources: got %v, want 3", f.Sources)
	}
	if len(f.Domain) != 2 || f.Domain[0] != "engineering" || f.Domain[1] != "testing" {
		t.Errorf("domain: got %v, want [engineering testing]", f.Domain)
	}
	if len(f.Entities) != 2 || f.Entities[0] != "Go" || f.Entities[1] != "knomit" {
		t.Errorf("entities: got %v, want [Go knomit]", f.Entities)
	}
	if f.EvidenceWeight != 0.7 {
		t.Errorf("evidence_weight: got %v, want 0.7", f.EvidenceWeight)
	}
}

func TestExplainResumeIncludesAllFactFields(t *testing.T) {
	// Regression: explainFactEntry in the resume path was also missing
	// domain, confidence, sources, entities, and evidence_weight.
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	sessionIdx := NewMockToolSessionIndex(ctrl)

	tmp := fact.NewFact("kb/ref.md")
	tmp.Title = "Ref Fact"
	tmp.Body = "Ref body."
	tmp.Type = fact.EpistemicType("hypothesis")
	tmp.Domain = []string{"infra"}
	tmp.Confidence = 0.6
	tmp.Sources = 2
	tmp.Entities = []string{"Redis"}
	tmp.Refs = []string{}
	tmp.EvidenceWeight = 0.5
	factContent := SerializeFact(tmp)

	sessionIdx.EXPECT().GetToolSession("sess-r").Return(&ToolSession{ID: "sess-r", Status: "active"}, nil)
	sessionIdx.EXPECT().GetSeenPaths("sess-r").Return(map[string]bool{}, nil)
	sessionIdx.EXPECT().DequeuePaths("sess-r", 25).Return([]QueueItem{
		{Path: "kb/ref.md", CommitHash: "cafebabe", Depth: 1},
	}, nil)
	gs.EXPECT().ReadFileAtCommit(testAgentBranch, "kb/ref.md", "cafebabe").Return(factContent, nil)
	sessionIdx.EXPECT().AddSeenPaths("sess-r", []string{"kb/ref.md"}).Return(nil)
	sessionIdx.EXPECT().EnqueuePaths(gomock.Any(), gomock.Any()).AnyTimes().Return(nil)
	sessionIdx.EXPECT().QueueSize("sess-r").Return(0, nil)
	sessionIdx.EXPECT().UpdateToolSession("sess-r", "", "completed").Return(nil)

	handler := ExplainHandler(gs, sessionIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":   "kb/ref.md",
		"cursor": "sess-r",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	var resp struct {
		Facts []explainFactEntry `json:"facts"`
	}
	if err := json.Unmarshal([]byte(getResultText(t, result)), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	f := resp.Facts[0]

	if f.Confidence != 0.6 {
		t.Errorf("confidence: got %v, want 0.6", f.Confidence)
	}
	if f.Sources != 2 {
		t.Errorf("sources: got %v, want 2", f.Sources)
	}
	if len(f.Domain) != 1 || f.Domain[0] != "infra" {
		t.Errorf("domain: got %v, want [infra]", f.Domain)
	}
	if len(f.Entities) != 1 || f.Entities[0] != "Redis" {
		t.Errorf("entities: got %v, want [Redis]", f.Entities)
	}
	if f.EvidenceWeight != 0.5 {
		t.Errorf("evidence_weight: got %v, want 0.5", f.EvidenceWeight)
	}
}
