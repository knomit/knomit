package mcp

import (
	"context"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
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
	// go-yaml v3 does not auto-coerce a scalar into []string, so this should
	// return a YAML parse error.
	content := "---\ndomain: testing\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Scalar Domain\n\nBody.\n"
	_, err := ParseFact("test/scalar-domain.md", content)
	if err == nil {
		t.Fatal("expected YAML error for scalar domain, got nil")
	}
}

func TestParseFactEntitiesAsString(t *testing.T) {
	// go-yaml v3 does not auto-coerce a scalar into []string, so this should
	// return a YAML parse error.
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
// ForgetHandler edge cases
// ---------------------------------------------------------------------------

func TestForgetEmptyFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := ForgetHandler(gs, idx, "know")

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

func TestForgetEmptyMomentName(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
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

func TestForgetFileExistsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/broken.md").Return(false, fmt.Errorf("git error"))

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/broken.md",
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

func TestForgetDeleteFileError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/fail.md").Return(true, nil)
	gs.EXPECT().DeleteFile("know/fail.md", gomock.Any()).Return(fmt.Errorf("delete failed"))

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/fail.md",
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

func TestForgetSyncError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, fmt.Errorf("sync failed"))

	handler := ForgetHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
		"moment_name": "test",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when Sync returns error")
	}
}

// ---------------------------------------------------------------------------
// UpdateHandler edge cases
// ---------------------------------------------------------------------------

func TestUpdateEmptyFile(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := UpdateHandler(gs, idx, "know")

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
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
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
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/broken.md").Return(false, fmt.Errorf("git error"))

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/broken.md",
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
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/fail.md").Return(true, nil)
	gs.EXPECT().ReadFile("know/fail.md").Return("", fmt.Errorf("read error"))

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/fail.md",
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

func TestUpdateSyncError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, fmt.Errorf("sync failed"))

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/foo.md",
		"moment_name": "test",
		"updates":     map[string]interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when Sync returns error")
	}
}

func TestUpdateTitleField(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	factContent := SerializeFact(Fact{
		Path: "know/title.md", Title: "Old Title", Body: "Body.",
		Domain: []string{}, Confidence: 0.5, Sources: 1,
		Entities: []string{}, Refs: []string{},
	})

	var writtenContent string

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/title.md").Return(true, nil)
	gs.EXPECT().ReadFile("know/title.md").Return(factContent, nil)
	gs.EXPECT().WriteFile("know/title.md", gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg string) error {
		writtenContent = content
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/title.md",
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

	updated, err := ParseFact("know/title.md", writtenContent)
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
	idx := NewMockSearchIndex(ctrl)

	factContent := SerializeFact(Fact{
		Path: "know/de.md", Title: "DE Test", Body: "Body.",
		Domain: []string{"old"}, Confidence: 0.5, Sources: 1,
		Entities: []string{"old-ent"}, Refs: []string{},
	})

	var writtenContent string

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().FileExists("know/de.md").Return(true, nil)
	gs.EXPECT().ReadFile("know/de.md").Return(factContent, nil)
	gs.EXPECT().WriteFile("know/de.md", gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg string) error {
		writtenContent = content
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	handler := UpdateHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"file":        "know/de.md",
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

	updated, err := ParseFact("know/de.md", writtenContent)
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

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)

	handler := LearnHandler(gs, idx, "know")

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

func TestLearnSyncError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, fmt.Errorf("sync failed"))

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test",
		"facts":       []interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error when Sync fails")
	}
}

func TestLearnBatchWriteError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).Return(fmt.Errorf("write failed"))

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "fail-write",
		"facts": []interface{}{
			map[string]interface{}{
				"path": "a", "title": "A", "body": "B.",
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

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).Return(nil)
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	// First Tag call fails (collision), second succeeds.
	gs.EXPECT().Tag("learn/collision").Return(fmt.Errorf("tag exists"))
	gs.EXPECT().Tag(gomock.Any()).Return(nil)

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "collision",
		"facts": []interface{}{
			map[string]interface{}{
				"path": "c", "title": "C", "body": "Body.",
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

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	gs.EXPECT().BatchWrite(gomock.Any(), gomock.Any()).DoAndReturn(func(files map[string]string, msg string) error {
		capturedFiles = files
		return nil
	})
	gs.EXPECT().HeadCommit().Return("abc123", nil)
	gs.EXPECT().Tag(gomock.Any()).Return(nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	handler := LearnHandler(gs, idx, "know")

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "nil-fields",
		"facts": []interface{}{
			map[string]interface{}{
				"path":       "niltest",
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

	// Verify the written content parses without error.
	content := capturedFiles["know/niltest.md"]
	if content == "" {
		t.Fatal("expected know/niltest.md to be written")
	}
	f, err := ParseFact("know/niltest.md", content)
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
