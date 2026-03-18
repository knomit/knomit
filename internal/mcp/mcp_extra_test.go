package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
)

// ---------------------------------------------------------------------------
// ParseFact edge cases
// ---------------------------------------------------------------------------

func TestParseFactEmptyBody(t *testing.T) {
	// Frontmatter present, title present, but no body after the heading.
	content := "---\ndomain: []\nconfidence: 0.5\nsources: 0\nentities: []\nrefs: []\n---\n# Title Only\n"
	f, err := ParseFact("test/empty-body.md", content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Title != "Title Only" {
		t.Fatalf("title: got %q want %q", f.Title, "Title Only")
	}
	if f.Body != "" {
		t.Fatalf("body: expected empty, got %q", f.Body)
	}
}

func TestParseFactConfidenceOutOfRange(t *testing.T) {
	// ParseFact should not reject out-of-range confidence; it just stores the value.
	content := "---\ndomain: []\nconfidence: 5.0\nsources: 0\nentities: []\nrefs: []\n---\n# Over Confidence\n\nBody.\n"
	f, err := ParseFact("test/high-conf.md", content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Confidence != 5.0 {
		t.Fatalf("confidence: got %v want 5.0", f.Confidence)
	}

	content2 := "---\ndomain: []\nconfidence: -1.0\nsources: 0\nentities: []\nrefs: []\n---\n# Negative Confidence\n\nBody.\n"
	f2, err := ParseFact("test/neg-conf.md", content2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f2.Confidence != -1.0 {
		t.Fatalf("confidence: got %v want -1.0", f2.Confidence)
	}
}

func TestParseFactDomainAsString(t *testing.T) {
	content := "---\ndomain: testing\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Scalar Domain\n\nBody.\n"
	_, err := ParseFact("test/scalar-domain.md", content)
	if err == nil {
		t.Fatal("expected YAML error for scalar domain, got nil")
	}
}

func TestParseFactEntitiesAsString(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.8\nsources: 1\nentities: foo\nrefs: []\n---\n# Scalar Entities\n\nBody.\n"
	_, err := ParseFact("test/scalar-ent.md", content)
	if err == nil {
		t.Fatal("expected YAML error for scalar entities, got nil")
	}
}

func TestParseFactMissingClosingDelimiter(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.5\n# No Closing\n\nBody.\n"
	_, err := ParseFact("test/no-close.md", content)
	if err == nil {
		t.Fatal("expected error for missing closing frontmatter delimiter")
	}
}

func TestParseFactEmptyTitleHeading(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.5\nsources: 0\nentities: []\nrefs: []\n---\n# \n\nBody.\n"
	_, err := ParseFact("test/empty-title.md", content)
	if err == nil {
		t.Fatal("expected error for empty title heading")
	}
}

func TestParseFactNoTitleHeading(t *testing.T) {
	content := "---\ndomain: []\nconfidence: 0.5\nsources: 0\nentities: []\nrefs: []\n---\nNo heading here.\n"
	_, err := ParseFact("test/no-heading.md", content)
	if err == nil {
		t.Fatal("expected error for missing title heading")
	}
}

// ---------------------------------------------------------------------------
// RetractHandler edge cases
// ---------------------------------------------------------------------------

func TestRetractEmptyFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	handler := RetractHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty file")
	}
}

func TestRetractEmptyMomentName(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	handler := RetractHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/technology/go/abc123.md",
		"moment_name": "",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty moment_name")
	}
}

func TestRetractFileExistsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().FileExists("kb/broken.md").Return(false, fmt.Errorf("git error"))

	handler := RetractHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/broken.md",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when FileExists returns error")
	}
}

func TestRetractDeleteFileError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().FileExists("kb/fail.md").Return(true, nil)
	gs.EXPECT().DeleteFile("kb/fail.md", gomock.Any(), gomock.Any()).Return("", fmt.Errorf("delete failed"))

	handler := RetractHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/fail.md",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when DeleteFile returns error")
	}
}

// ---------------------------------------------------------------------------
// UpdateHandler edge cases
// ---------------------------------------------------------------------------

func TestUpdateEmptyFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty file")
	}
}

func TestUpdateEmptyMomentName(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)


	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/technology/go/abc.md",
		"moment_name": "",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty moment_name")
	}
}

func TestUpdateFileExistsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().FileExists("kb/broken.md").Return(false, fmt.Errorf("git error"))

	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/broken.md",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when FileExists returns error")
	}
}

func TestUpdateReadFileError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().FileExists("kb/fail.md").Return(true, nil)
	gs.EXPECT().ReadFile("kb/fail.md").Return("", fmt.Errorf("read error"))

	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/fail.md",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when ReadFile returns error")
	}
}



func TestUpdateTitleField(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	factContent := SerializeFact(Fact{
		Path: "kb/technology/go/abc.md", Title: "Old Title", Body: "Body.",
		Domain: []string{}, Confidence: 0.5, Sources: 1,
		Entities: []string{}, Refs: []string{},
	})

	var writtenContent string


	gs.EXPECT().FileExists("kb/technology/go/abc.md").Return(true, nil)
	gs.EXPECT().ReadFile("kb/technology/go/abc.md").Return(factContent, nil)
	gs.EXPECT().WriteFile("kb/technology/go/abc.md", gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg, operation string) (string, string, error) {
		writtenContent = content
		return "abc123", "blob_title", nil
	})

	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/technology/go/abc.md",
		"moment_name": "title-update",
		"updates": map[string]interface{}{
			"title": "New Title",
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	updated, err := ParseFact("kb/technology/go/abc.md", writtenContent)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if updated.Title != "New Title" {
		t.Fatalf("title: got %q want %q", updated.Title, "New Title")
	}
	// Body should remain unchanged.
	if updated.Body != "Body." {
		t.Fatalf("body should be unchanged: got %q", updated.Body)
	}
}

func TestUpdateDomainAndEntities(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	factContent := SerializeFact(Fact{
		Path: "kb/technology/go/abc.md", Title: "DE Test", Body: "Body.",
		Domain: []string{"old"}, Confidence: 0.5, Sources: 1,
		Entities: []string{"old-ent"}, Refs: []string{},
	})

	var writtenContent string


	gs.EXPECT().FileExists("kb/technology/go/abc.md").Return(true, nil)
	gs.EXPECT().ReadFile("kb/technology/go/abc.md").Return(factContent, nil)
	gs.EXPECT().WriteFile("kb/technology/go/abc.md", gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg, operation string) (string, string, error) {
		writtenContent = content
		return "abc123", "blob_de", nil
	})

	handler := UpdateHandler(gs, "kb")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "kb/technology/go/abc.md",
		"moment_name": "de-update",
		"updates": map[string]interface{}{
			"domain":   []interface{}{"new-domain"},
			"entities": []interface{}{"new-ent-a", "new-ent-b"},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	updated, err := ParseFact("kb/technology/go/abc.md", writtenContent)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updated.Domain) != 1 || updated.Domain[0] != "new-domain" {
		t.Fatalf("domain: got %v want [new-domain]", updated.Domain)
	}
	if len(updated.Entities) != 2 {
		t.Fatalf("entities: got %v want 2 elements", updated.Entities)
	}
}

// ---------------------------------------------------------------------------
// LearnHandler edge cases
// ---------------------------------------------------------------------------

func TestLearnEmptyFacts(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)



	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology())

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "empty",
		"facts":       []interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty facts array")
	}
}

func TestLearnBatchWriteError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)


	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any(), gomock.Any()).Return("", nil, fmt.Errorf("write failed"))

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology())

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "fail-write",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "technology", "category": "go", "title": "A", "body": "B.",
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
		t.Fatal("expected tool error when BatchWrite fails")
	}
}

func TestLearnTagCollision(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)


	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg, operation string) (string, map[string]string, error) {
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "abc123", blobHashes, nil
	})
	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology())

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "collision",
		"facts": []interface{}{
			map[string]interface{}{
				"topic": "science", "category": "physics", "title": "C", "body": "Body.",
				"domain": []interface{}{}, "confidence": 0.5, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
}

func TestLearnNilDomainEntitiesRefs(t *testing.T) {
	// When domain/entities/refs are omitted from JSON, they come through as nil.
	// LearnHandler should default them to empty slices.
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var capturedFiles map[string]string


	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg, operation string) (string, map[string]string, error) {
		capturedFiles = files
		blobHashes := make(map[string]string, len(files))
		for path := range files {
			blobHashes[path] = "blob_" + path
		}
		return "abc123", blobHashes, nil
	})

	handler := LearnHandler(gs, idx, "kb", fact.DefaultOntology())

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "nil-fields",
		"facts": []interface{}{
			map[string]interface{}{
				"topic":      "people",
				"category":   "alice",
				"title":      "Nil Fields",
				"body":       "Body.",
				"confidence": 0.5,
				"sources":    1,
				// domain, entities, refs intentionally omitted
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

	// Find the written file (path has UUID).
	var content string
	var writtenPath string
	for path, c := range capturedFiles {
		if strings.HasPrefix(path, "kb/people/alice/") {
			content = c
			writtenPath = path
		}
	}
	if writtenPath == "" {
		t.Fatal("expected kb/people/alice/ file to be written")
	}
	f, err := ParseFact(writtenPath, content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if f.Domain == nil {
		t.Fatal("domain should be non-nil empty slice")
	}
	if f.Entities == nil {
		t.Fatal("entities should be non-nil empty slice")
	}
	if f.Refs == nil {
		t.Fatal("refs should be non-nil empty slice")
	}
}
