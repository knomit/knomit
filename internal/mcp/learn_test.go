package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
)

func TestLearnWritesFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var capturedFiles map[string]string
	var capturedUpsert FactRecord

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg string) error {
		capturedFiles = files
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123def456", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).DoAndReturn(func(r FactRecord) error {
		capturedUpsert = r
		return nil
	})

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test-moment",
		"facts": []interface{}{
			map[string]interface{}{
				"path":       "test/foo",
				"title":      "Test Fact",
				"body":       "Some body text.",
				"domain":     []interface{}{"testing"},
				"confidence": 0.9,
				"sources":    1,
				"entities":   []interface{}{"foo"},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// Verify file was written with normalized path.
	expectedPath := "know/test/foo.md"
	if _, ok := capturedFiles[expectedPath]; !ok {
		t.Fatalf("expected file %q to be written; written: %v", expectedPath, capturedFiles)
	}

	// Verify the file content parses correctly.
	content := capturedFiles[expectedPath]
	fact, err := ParseFact(expectedPath, content)
	if err != nil {
		t.Fatalf("written file does not parse: %v", err)
	}
	if fact.Title != "Test Fact" {
		t.Fatalf("title: got %q want %q", fact.Title, "Test Fact")
	}

	// Verify result JSON has moment_tag.
	textContent := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(textContent), &resp); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, textContent)
	}
	tag, _ := resp["moment_tag"].(string)
	if !strings.HasPrefix(tag, "learn/test-moment") {
		t.Fatalf("moment_tag: got %q want prefix learn/test-moment", tag)
	}

	// Verify index was updated.
	if capturedUpsert.Path != expectedPath {
		t.Fatalf("upserted path: got %q want %q", capturedUpsert.Path, expectedPath)
	}
}

func TestLearnNormalizesPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var capturedFiles map[string]string

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg string) error {
		capturedFiles = files
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123def456", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "path-test",
		"facts": []interface{}{
			map[string]interface{}{
				"path":       "know/already/normalized.md",
				"title":      "Normalized",
				"body":       "",
				"domain":     []interface{}{},
				"confidence": 0.5,
				"sources":    0,
				"entities":   []interface{}{},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Already-normalized path should not be doubled.
	if _, ok := capturedFiles["know/already/normalized.md"]; !ok {
		t.Fatalf("expected know/already/normalized.md in written, got: %v", capturedFiles)
	}
	if _, ok := capturedFiles["know/know/already/normalized.md"]; ok {
		t.Fatal("path was incorrectly double-prefixed")
	}
}

func TestLearnRequiresMomentName(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"facts": []interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing moment_name")
	}
}

func TestLearnMultipleFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var capturedFiles map[string]string

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg string) error {
		capturedFiles = files
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123def456", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil).Times(2)

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "multi",
		"facts": []interface{}{
			map[string]interface{}{
				"path": "a", "title": "Fact A", "body": "A body.",
				"domain": []interface{}{}, "confidence": 0.8, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
			map[string]interface{}{
				"path": "b", "title": "Fact B", "body": "B body.",
				"domain": []interface{}{}, "confidence": 0.7, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if _, ok := capturedFiles["know/a.md"]; !ok {
		t.Error("missing know/a.md")
	}
	if _, ok := capturedFiles["know/b.md"]; !ok {
		t.Error("missing know/b.md")
	}
}

// getResultText extracts the text content from a CallToolResult.
func getResultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := mcpgo.AsTextContent(c); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}
