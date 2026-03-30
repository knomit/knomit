package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/store"
)

func TestHypothesizeFirstCallReturnsSynthesisFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("abc123", nil).AnyTimes()

	// No watermark → first run.
	pIdx.EXPECT().GCPipelineSessions(gomock.Any(), "hypothesize", "machine/test", 5).Return(nil)
	pIdx.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test").Return("", nil)

	// Return one synthesis fact and one observation (should be filtered by IncludeTypes).
	synthResult := SearchResult{
		FactWithBody: FactWithBody{
			FactRecord: FactRecord{
				Path:       "kb/tech/go/synth-1.md",
				Title:      "Go concurrency patterns",
				Type:       "synthesis",
				Domain:     []string{"go"},
				Confidence: 0.85,
				Sources:    3,
				Entities:   []string{"goroutine"},
			},
			Body: "Synthesis of concurrency patterns.",
		},
		Score: 100,
	}
	idx.EXPECT().Search(gomock.Any(), gomock.Any(), SearchQuery{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return([]SearchResult{synthResult}, nil)

	// Create session.
	sess := &store.PipelineSession{ID: "sess-1", Status: "active"}
	pIdx.EXPECT().CreatePipelineSession(gomock.Any(), "hypothesize", "machine/test").Return(sess, nil)

	// Insert work item.
	pIdx.EXPECT().InsertPipelineWorkItem(gomock.Any(), gomock.Any()).Return(nil)

	// NextPipelineWorkItem for hypothesizeNextItem.
	factJSON, _ := json.Marshal(map[string]interface{}{
		"path":       "kb/tech/go/synth-1.md",
		"title":      "Go concurrency patterns",
		"body":       "Synthesis of concurrency patterns.",
		"type":       "synthesis",
		"domain":     []string{"go"},
		"confidence": 0.85,
		"sources":    3,
		"entities":   []string{"goroutine"},
	})
	workItem := &store.PipelineWorkItem{
		ID:         1,
		SessionID:  "sess-1",
		StepType:   "hypothesize",
		ClusterKey: "synth-0",
		FactsJSON:  string(factJSON),
		Priority:   1,
	}
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-1").Return(workItem, nil)
	pIdx.EXPECT().PipelineWorkItemStats(gomock.Any(), "sess-1").Return(0, 1, nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// Parse the response.
	text := result.Content[0].(mcpgo.TextContent).Text
	var resp HypothesizeResult
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Done {
		t.Fatal("expected done=false, got true")
	}
	if resp.SessionID != "sess-1" {
		t.Fatalf("expected session_id=sess-1, got %q", resp.SessionID)
	}
	if resp.Item == nil {
		t.Fatal("expected item, got nil")
	}
	if resp.Item.Type != "hypothesize" {
		t.Fatalf("expected item type=hypothesize, got %q", resp.Item.Type)
	}
	if resp.Progress == nil || resp.Progress.Remaining != 1 {
		t.Fatalf("expected remaining=1, got %+v", resp.Progress)
	}
}

func TestHypothesizeEmptySession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("abc123", nil).AnyTimes()

	pIdx.EXPECT().GCPipelineSessions(gomock.Any(), "hypothesize", "machine/test", 5).Return(nil)
	pIdx.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test").Return("", nil)

	// No synthesis facts.
	idx.EXPECT().Search(gomock.Any(), gomock.Any(), SearchQuery{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(nil, nil)

	// Should advance watermark.
	pIdx.EXPECT().SetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test", "abc123").Return(nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	text := result.Content[0].(mcpgo.TextContent).Text
	var resp HypothesizeResult
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if !resp.Done {
		t.Fatal("expected done=true, got false")
	}
}

func TestHypothesizeContinueSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("def456", nil).AnyTimes()

	// Step 1: Start a session with 2 facts.
	pIdx.EXPECT().GCPipelineSessions(gomock.Any(), "hypothesize", "machine/test", 5).Return(nil)
	pIdx.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test").Return("", nil)

	results := []SearchResult{
		{
			FactWithBody: FactWithBody{
				FactRecord: FactRecord{
					Path:  "kb/a.md",
					Title: "Fact A",
					Type:  "synthesis",
				},
				Body: "Body A",
			},
			Score: 100,
		},
		{
			FactWithBody: FactWithBody{
				FactRecord: FactRecord{
					Path:  "kb/b.md",
					Title: "Fact B",
					Type:  "synthesis",
				},
				Body: "Body B",
			},
			Score: 100,
		},
	}
	idx.EXPECT().Search(gomock.Any(), gomock.Any(), gomock.Any()).Return(results, nil)

	sess := &store.PipelineSession{ID: "sess-2", Status: "active"}
	pIdx.EXPECT().CreatePipelineSession(gomock.Any(), "hypothesize", "machine/test").Return(sess, nil)
	pIdx.EXPECT().InsertPipelineWorkItem(gomock.Any(), gomock.Any()).Return(nil).Times(2)

	// First item returned by hypothesizeNextItem (called from hypothesizeStart).
	factAJSON, _ := json.Marshal(map[string]interface{}{
		"path": "kb/a.md", "title": "Fact A", "body": "Body A", "type": "synthesis",
	})
	workItemA := &store.PipelineWorkItem{
		ID: 1, SessionID: "sess-2", StepType: "hypothesize",
		ClusterKey: "synth-0", FactsJSON: string(factAJSON), Priority: 2,
	}
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-2").Return(workItemA, nil)
	pIdx.EXPECT().PipelineWorkItemStats(gomock.Any(), "sess-2").Return(0, 2, nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	startReq := mcpgo.CallToolRequest{}
	startReq.Params.Arguments = map[string]interface{}{}
	startResult, err := handler(context.Background(), startReq)
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	text := startResult.Content[0].(mcpgo.TextContent).Text
	var startResp HypothesizeResult
	json.Unmarshal([]byte(text), &startResp)
	if startResp.Done {
		t.Fatal("expected start to not be done")
	}

	// Step 2: Continue the session — should acknowledge item A and get item B.
	pIdx.EXPECT().GetPipelineSession(gomock.Any(), "sess-2").Return(sess, nil)

	// NextPipelineWorkItem returns item A (current unanswered) to be acknowledged.
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-2").Return(workItemA, nil)
	pIdx.EXPECT().SetPipelineWorkItemResponse(gomock.Any(), int64(1), "acknowledged").Return(nil)

	// NextPipelineWorkItem returns item B (the next one).
	factBJSON, _ := json.Marshal(map[string]interface{}{
		"path": "kb/b.md", "title": "Fact B", "body": "Body B", "type": "synthesis",
	})
	workItemB := &store.PipelineWorkItem{
		ID: 2, SessionID: "sess-2", StepType: "hypothesize",
		ClusterKey: "synth-1", FactsJSON: string(factBJSON), Priority: 1,
	}
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-2").Return(workItemB, nil)
	pIdx.EXPECT().PipelineWorkItemStats(gomock.Any(), "sess-2").Return(1, 1, nil)

	contReq := mcpgo.CallToolRequest{}
	contReq.Params.Arguments = map[string]interface{}{
		"session_id": "sess-2",
	}
	contResult, err := handler(context.Background(), contReq)
	if err != nil {
		t.Fatalf("continue error: %v", err)
	}

	text = contResult.Content[0].(mcpgo.TextContent).Text
	var contResp HypothesizeResult
	json.Unmarshal([]byte(text), &contResp)
	if contResp.Done {
		t.Fatal("expected continue to not be done")
	}
	if contResp.Progress.Completed != 1 {
		t.Fatalf("expected completed=1, got %d", contResp.Progress.Completed)
	}
	if contResp.Progress.Remaining != 1 {
		t.Fatalf("expected remaining=1, got %d", contResp.Progress.Remaining)
	}
}

func TestHypothesizeContinueSessionNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	pIdx.EXPECT().GetPipelineSession(gomock.Any(), "nonexistent").Return(nil, nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "nonexistent",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for nonexistent session")
	}
}

func TestHypothesizeIncrementalWithWatermark(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("def456", nil).AnyTimes()

	pIdx.EXPECT().GCPipelineSessions(gomock.Any(), "hypothesize", "machine/test", 5).Return(nil)
	pIdx.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test").Return("abc123", nil)

	// DiffFiles returns one added synthesis .md and one non-.md file.
	gs.EXPECT().DiffFiles(gomock.Any(), testAgentBranch, "abc123").Return(
		[]string{"kb/tech/new-synth.md", "kb/tech/data.json"},
		[]string{},
		[]string{},
		nil,
	)

	synthContent := `---
type: synthesis
domain: [go]
confidence: 0.9
sources: 2
entities: [channels]
refs: []
---
# New synthesis

Synthesized insight about channels.
`
	gs.EXPECT().ReadFile(gomock.Any(), testAgentBranch, "kb/tech/new-synth.md").Return(synthContent, nil)
	// .json file should be skipped (not .md).

	sess := &store.PipelineSession{ID: "sess-3", Status: "active"}
	pIdx.EXPECT().CreatePipelineSession(gomock.Any(), "hypothesize", "machine/test").Return(sess, nil)
	pIdx.EXPECT().InsertPipelineWorkItem(gomock.Any(), gomock.Any()).Return(nil)

	factJSON, _ := json.Marshal(map[string]interface{}{
		"path": "kb/tech/new-synth.md", "title": "New synthesis",
	})
	workItem := &store.PipelineWorkItem{
		ID: 1, SessionID: "sess-3", StepType: "hypothesize",
		FactsJSON: string(factJSON), Priority: 1,
	}
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-3").Return(workItem, nil)
	pIdx.EXPECT().PipelineWorkItemStats(gomock.Any(), "sess-3").Return(0, 1, nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	text := result.Content[0].(mcpgo.TextContent).Text
	var resp HypothesizeResult
	json.Unmarshal([]byte(text), &resp)
	if resp.Done {
		t.Fatal("expected done=false")
	}
	if resp.SessionID != "sess-3" {
		t.Fatalf("expected session_id=sess-3, got %q", resp.SessionID)
	}
}

func TestHypothesizeContinueExpiredSession(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	sess := &store.PipelineSession{ID: "sess-expired", Status: "completed"}
	pIdx.EXPECT().GetPipelineSession(gomock.Any(), "sess-expired").Return(sess, nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "sess-expired",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for completed session")
	}
}

func TestHypothesizeContinueNilCurrentItem(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("abc123", nil).AnyTimes()

	sess := &store.PipelineSession{ID: "sess-nil", Status: "active"}
	pIdx.EXPECT().GetPipelineSession(gomock.Any(), "sess-nil").Return(sess, nil)

	// First NextPipelineWorkItem call in hypothesizeContinue returns nil (nothing to mark).
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-nil").Return(nil, nil)

	// Second NextPipelineWorkItem call in hypothesizeNextItem also returns nil → complete.
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-nil").Return(nil, nil)
	pIdx.EXPECT().CompletePipelineSession(gomock.Any(), "sess-nil").Return(nil)
	pIdx.EXPECT().SetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test", "abc123").Return(nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "sess-nil",
		"response":   "ack",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	text := result.Content[0].(mcpgo.TextContent).Text
	var resp HypothesizeResult
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if !resp.Done {
		t.Fatal("expected done=true when no items to mark")
	}
}

func TestHypothesizeStartSearchError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg

	pIdx.EXPECT().GCPipelineSessions(gomock.Any(), "hypothesize", "machine/test", 5).Return(nil)
	pIdx.EXPECT().GetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test").Return("", nil)

	// Search returns an error.
	idx.EXPECT().Search(gomock.Any(), gomock.Any(), SearchQuery{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	}).Return(nil, fmt.Errorf("database locked"))

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when search fails")
	}
}

func TestHypothesizeSessionCompletes(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	pIdx := NewMockPipelineIndex(ctrl)

	// branch from handler arg
	gs.EXPECT().HeadCommit(gomock.Any(), testAgentBranch).Return("final789", nil).AnyTimes()

	sess := &store.PipelineSession{ID: "sess-done", Status: "active"}
	pIdx.EXPECT().GetPipelineSession(gomock.Any(), "sess-done").Return(sess, nil)

	// Current item to acknowledge.
	workItem := &store.PipelineWorkItem{ID: 5, SessionID: "sess-done"}
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-done").Return(workItem, nil)
	pIdx.EXPECT().SetPipelineWorkItemResponse(gomock.Any(), int64(5), "done processing").Return(nil)

	// No more items.
	pIdx.EXPECT().NextPipelineWorkItem(gomock.Any(), "sess-done").Return(nil, nil)
	pIdx.EXPECT().CompletePipelineSession(gomock.Any(), "sess-done").Return(nil)
	pIdx.EXPECT().SetPipelineWatermark(gomock.Any(), "hypothesize", "machine/test", "final789").Return(nil)

	handler := HypothesizeHandler(gs, idx, pIdx, "kb", testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"session_id": "sess-done",
		"response":   "done processing",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	text := result.Content[0].(mcpgo.TextContent).Text
	var resp HypothesizeResult
	json.Unmarshal([]byte(text), &resp)
	if !resp.Done {
		t.Fatal("expected done=true")
	}
	if resp.SessionID != "sess-done" {
		t.Fatalf("expected session_id=sess-done, got %q", resp.SessionID)
	}
}
