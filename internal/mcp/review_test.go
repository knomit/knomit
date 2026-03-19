package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// stubReviewer implements Reviewer for testing.
type stubReviewer struct {
	startResult    *ReviewResult
	startErr       error
	continueResult *ReviewResult
	continueErr    error
}

func (s *stubReviewer) StartSession() (*ReviewResult, error) {
	return s.startResult, s.startErr
}

func (s *stubReviewer) ContinueSession(sessionID, response string) (*ReviewResult, error) {
	return s.continueResult, s.continueErr
}

func callReviewHandler(t *testing.T, reviewer Reviewer, params map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	handler := ReviewHandler(reviewer)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = params
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return result
}

func TestReviewHandler_StartSession(t *testing.T) {
	reviewer := &stubReviewer{
		startResult: &ReviewResult{
			SessionID: "sess-1",
			Item:      &ReviewItem{Type: "prune", Prompt: "Review these facts."},
			Progress:  &ReviewProgress{Completed: 0, Remaining: 3},
		},
	}

	result := callReviewHandler(t, reviewer, map[string]any{})

	if result.IsError {
		t.Fatalf("expected success, got error result")
	}

	text := extractText(t, result)
	var got ReviewResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", got.SessionID)
	}
	if got.Item == nil || got.Item.Type != "prune" {
		t.Errorf("item.type = %v, want prune", got.Item)
	}
}

func TestReviewHandler_ContinueSession(t *testing.T) {
	reviewer := &stubReviewer{
		continueResult: &ReviewResult{
			SessionID: "sess-1",
			Done:      true,
			Summary:   &ReviewStats{Pruned: 2},
		},
	}

	result := callReviewHandler(t, reviewer, map[string]any{
		"session_id": "sess-1",
		"response":   `{"decisions":[]}`,
	})

	if result.IsError {
		t.Fatalf("expected success, got error result")
	}

	text := extractText(t, result)
	var got ReviewResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Done {
		t.Error("expected done=true")
	}
}

func TestReviewHandler_ContinueWithoutResponse(t *testing.T) {
	reviewer := &stubReviewer{}

	result := callReviewHandler(t, reviewer, map[string]any{
		"session_id": "sess-1",
		// no "response" key
	})

	if !result.IsError {
		t.Fatal("expected error result when response is missing")
	}
	text := extractText(t, result)
	if !strings.Contains(text, "response is required") {
		t.Errorf("unexpected error message: %q", text)
	}
}

func TestReviewHandler_StartError(t *testing.T) {
	reviewer := &stubReviewer{startErr: fmt.Errorf("db unavailable")}

	result := callReviewHandler(t, reviewer, map[string]any{})

	if !result.IsError {
		t.Fatal("expected error result")
	}
	text := extractText(t, result)
	if !strings.Contains(text, "db unavailable") {
		t.Errorf("unexpected error message: %q", text)
	}
}

func TestReviewHandler_ContinueError(t *testing.T) {
	reviewer := &stubReviewer{continueErr: fmt.Errorf("session expired")}

	result := callReviewHandler(t, reviewer, map[string]any{
		"session_id": "sess-x",
		"response":   "{}",
	})

	if !result.IsError {
		t.Fatal("expected error result")
	}
	text := extractText(t, result)
	if !strings.Contains(text, "session expired") {
		t.Errorf("unexpected error message: %q", text)
	}
}

// extractText returns the text content from a CallToolResult.
func extractText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}
