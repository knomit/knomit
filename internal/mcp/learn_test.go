package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
)

func TestLearnWritesFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var capturedFiles map[string]string


	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(testAgentBranch, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, files map[string]string, msg, operation string) (string, map[string]string, error) {
		capturedFiles = files
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "abc123def456", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test-moment",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":      "technology",
				"category":   "go/testing",
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

	// Verify file was written with correct path prefix (UUID is random).
	expectedPrefix := "kb/technology/go/testing/"
	var writtenPath string
	for path := range capturedFiles {
		if strings.HasPrefix(path, expectedPrefix) {
			writtenPath = path
		}
	}
	if writtenPath == "" {
		t.Fatalf("expected file with prefix %q to be written; written: %v", expectedPrefix, capturedFiles)
	}
	if !strings.HasSuffix(writtenPath, ".md") {
		t.Fatalf("expected .md suffix, got %q", writtenPath)
	}

	// Verify the file content parses correctly.
	content := capturedFiles[writtenPath]
	fact, err := ParseFact(writtenPath, content)
	if err != nil {
		t.Fatalf("written file does not parse: %v", err)
	}
	if fact.Title != "Test Fact" {
		t.Fatalf("title: got %q want %q", fact.Title, "Test Fact")
	}

}

func TestLearnRequiresTopic(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)



	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":      "banana",
				"category":   "foo",
				"title":      "Bad Topic",
				"body":       "Body.",
				"domain":     []interface{}{},
				"confidence": 0.5,
				"sources":    1,
				"entities":   []interface{}{},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for invalid topic")
	}
}

func TestLearnRequiresCategory(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)



	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":      "technology",
				"category":   "",
				"title":      "No Category",
				"body":       "Body.",
				"domain":     []interface{}{},
				"confidence": 0.5,
				"sources":    1,
				"entities":   []interface{}{},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty category")
	}
}

func TestLearnRequiresMomentName(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)



	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

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


	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(testAgentBranch, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, files map[string]string, msg, operation string) (string, map[string]string, error) {
		capturedFiles = files
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "abc123def456", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "multi",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "science", "category": "physics", "title": "Fact A", "body": "A body.",
				"domain": []interface{}{}, "confidence": 0.8, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
			map[string]interface{}{
				"topic": "people", "category": "alice", "title": "Fact B", "body": "B body.",
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

	// Verify two files were written to different topic paths.
	var foundScience, foundPeople bool
	for path := range capturedFiles {
		if strings.HasPrefix(path, "kb/science/physics/") {
			foundScience = true
		}
		if strings.HasPrefix(path, "kb/people/alice/") {
			foundPeople = true
		}
	}
	if !foundScience {
		t.Error("missing kb/science/physics/ file")
	}
	if !foundPeople {
		t.Error("missing kb/people/alice/ file")
	}
}

func TestLearnHandler_DedupMergesNearDuplicate(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)



	// Search returns an existing near-duplicate (score=95)
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]SearchResult{
		{FactWithBody: FactWithBody{
			FactRecord: FactRecord{
				Path:       "kb/technology/cameras/abc123.md",
				Title:      "Camera Review",
				Domain:     []string{"tech"},
				Entities:   []string{"camera"},
				Confidence: 0.8,
				Sources:    1,
				Refs:       []string{},
			},
			Body: "Great camera with clear video",
		}, Score: 95},
	}, nil)

	// Read existing fact to get full content
	gs.EXPECT().ReadFile(testAgentBranch, "kb/technology/cameras/abc123.md").Return(
		"---\ndomain: [tech]\nconfidence: 0.8\nsources: 1\nentities: [camera]\nrefs: []\n---\n# Camera Review\n\nGreat camera with clear video\n", nil)

	// BatchWrite should write to existing path (merged)
	var capturedFiles map[string]string
	gs.EXPECT().BatchWrite(testAgentBranch, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, files map[string]string, msg, operation string) (string, map[string]string, error) {
		capturedFiles = files
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "commit_merged", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "dedup-test",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":      "technology",
				"category":   "cameras",
				"title":      "Camera Assessment",
				"body":       "Camera provides clear video quality",
				"domain":     []interface{}{"hardware"},
				"confidence": 0.9,
				"sources":    1,
				"entities":   []interface{}{"camera"},
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

	// Should write to existing path, not a new UUID path
	if _, ok := capturedFiles["kb/technology/cameras/abc123.md"]; !ok {
		t.Fatalf("expected write to kb/technology/cameras/abc123.md, got: %v", capturedFiles)
	}

	// Verify merged content written to the existing path.
	mergedContent := capturedFiles["kb/technology/cameras/abc123.md"]
	mergedFact, err := ParseFact("kb/technology/cameras/abc123.md", mergedContent)
	if err != nil {
		t.Fatalf("parse merged fact: %v", err)
	}
	if mergedFact.Confidence != 0.9 {
		t.Errorf("confidence: got %v, want 0.9", mergedFact.Confidence)
	}
	if mergedFact.Sources != 2 {
		t.Errorf("sources: got %d, want 2", mergedFact.Sources)
	}
}

func TestBuildFactPath(t *testing.T) {
	path := fact.BuildFactPath("kb", "technology", "go/concurrency")
	if !strings.HasPrefix(path, "kb/technology/go/concurrency/") {
		t.Fatalf("expected prefix kb/technology/go/concurrency/, got %q", path)
	}
	if !strings.HasSuffix(path, ".md") {
		t.Fatalf("expected .md suffix, got %q", path)
	}
	// UUID portion should be 8 chars.
	parts := strings.Split(path, "/")
	leaf := strings.TrimSuffix(parts[len(parts)-1], ".md")
	if len(leaf) != 8 {
		t.Fatalf("expected 8-char UUID leaf, got %q (%d chars)", leaf, len(leaf))
	}

	// Two calls should produce different paths.
	path2 := fact.BuildFactPath("kb", "technology", "go/concurrency")
	if path == path2 {
		t.Fatal("expected different UUIDs for different calls")
	}
}

func TestLearnBatchRejectsMixedTypes(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "mixed-batch",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "technology", "category": "go/testing",
				"title": "Observed Fact", "body": "Something observed.",
				"type": "observation",
				"domain": []interface{}{}, "confidence": 0.8, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
			map[string]interface{}{
				"topic": "technology", "category": "go/testing",
				"title": "Hypothesis Fact", "body": "A prediction.",
				"type": "hypothesis",
				"domain": []interface{}{}, "confidence": 0.5, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for mixed observed/inferred batch")
	}
	text := getResultText(t, result)
	if !strings.Contains(text, "cannot mix") {
		t.Fatalf("expected 'cannot mix' in error, got: %s", text)
	}
}

func TestLearnBatchAllowsMultipleHypotheses(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(testAgentBranch, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, files map[string]string, msg, operation string) (string, map[string]string, error) {
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "abc123", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "hypo-batch",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "technology", "category": "go/testing",
				"title": "Hypothesis A", "body": "Prediction A.",
				"type": "hypothesis",
				"domain": []interface{}{}, "confidence": 0.5, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
			map[string]interface{}{
				"topic": "technology", "category": "go/testing",
				"title": "Hypothesis B", "body": "Prediction B.",
				"type": "hypothesis",
				"domain": []interface{}{}, "confidence": 0.4, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success for all-hypothesis batch, got error: %v", result.Content)
	}
}

func TestLearnDedupObservationSubsumesHypothesis(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	// Search returns an existing hypothesis as near-duplicate.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]SearchResult{
		{FactWithBody: FactWithBody{
			FactRecord: FactRecord{
				Path:       "kb/technology/go/testing/existing.md",
				Title:      "Testing Hypothesis",
				Type:       "hypothesis",
				Domain:     []string{"testing"},
				Entities:   []string{},
				Confidence: 0.5,
				Sources:    1,
				Refs:       []string{},
			},
			Body: "I predict tests will pass",
		}, Score: 95},
	}, nil)

	// Read existing hypothesis fact.
	gs.EXPECT().ReadFile(testAgentBranch, "kb/technology/go/testing/existing.md").Return(
		"---\ntype: hypothesis\ndomain: [testing]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\n---\n# Testing Hypothesis\n\nI predict tests will pass\n", nil)

	// Expect the hypothesis to be deleted.
	gs.EXPECT().DeleteFile(testAgentBranch, "kb/technology/go/testing/existing.md", gomock.Any(), "retract").Return("del123", nil)

	// BatchWrite should write the observation (not merged into existing path).
	var capturedFiles map[string]string
	gs.EXPECT().BatchWrite(testAgentBranch, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, files map[string]string, msg, operation string) (string, map[string]string, error) {
		capturedFiles = files
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "obs123", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology(), testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "subsume-test",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "technology", "category": "go/testing",
				"title": "Tests Pass", "body": "Tests actually pass.",
				"type": "observation",
				"domain": []interface{}{"testing"}, "confidence": 0.9, "sources": 1,
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

	// The observation should be written to a new path (not the hypothesis path).
	if _, ok := capturedFiles["kb/technology/go/testing/existing.md"]; ok {
		t.Error("observation should NOT be written to the hypothesis path")
	}

	// Verify the written observation includes the hypothesis path in refs.
	for path, content := range capturedFiles {
		if strings.HasPrefix(path, "kb/technology/go/testing/") {
			f, err := ParseFact(path, content)
			if err != nil {
				t.Fatalf("parse written fact: %v", err)
			}
			found := false
			for _, ref := range f.Refs {
				if ref == "kb/technology/go/testing/existing.md" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected hypothesis path in refs, got: %v", f.Refs)
			}
		}
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
