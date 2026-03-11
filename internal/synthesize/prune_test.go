package synthesize

import (
	"context"
	"strings"
	"testing"

	"knomit/internal/llm"
	"knomit/internal/store"
)

// mockLLM is a test LLM adapter that returns a fixed response.
type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) Complete(_ context.Context, _ string, _ []llm.Message, _ func(string)) (string, error) {
	return m.response, m.err
}

// mockGitStore is a minimal GitStore for synthesize tests.
type mockGitStore struct {
	files   map[string]string // path → content
	written map[string]string
	deleted []string
	tags    []string
}

func newMockGitStore() *mockGitStore {
	return &mockGitStore{
		files:   map[string]string{},
		written: map[string]string{},
	}
}

func (m *mockGitStore) ReadFile(path string) (string, error) {
	if c, ok := m.written[path]; ok {
		return c, nil
	}
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	return "", &notFoundError{path}
}

type notFoundError struct{ path string }

func (e *notFoundError) Error() string { return "not found: " + e.path }

func (m *mockGitStore) WriteFile(path, content, _ string) error {
	m.written[path] = content
	return nil
}

func (m *mockGitStore) BatchWrite(files map[string]string, _ string) error {
	for k, v := range files {
		m.written[k] = v
	}
	return nil
}

func (m *mockGitStore) DeleteFile(path, _ string) error {
	m.deleted = append(m.deleted, path)
	delete(m.written, path)
	delete(m.files, path)
	return nil
}

func (m *mockGitStore) ListAll() ([]string, error) {
	var paths []string
	for p := range m.files {
		paths = append(paths, p)
	}
	for p := range m.written {
		// avoid duplicates
		if _, ok := m.files[p]; !ok {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (m *mockGitStore) HeadCommit() (string, error) {
	return "deadbeef", nil
}

func (m *mockGitStore) Tag(name string) error {
	m.tags = append(m.tags, name)
	return nil
}

func (m *mockGitStore) Branch() string {
	return "machine/test"
}

func (m *mockGitStore) DiffFiles(_ string) (added, modified, deleted []string, err error) {
	return nil, nil, nil, nil
}

// mockSearchIndex is a minimal SearchIndex for synthesize tests.
type mockSearchIndex struct {
	upserted []store.FactRecord
	deleted  []string
}

func (m *mockSearchIndex) Search(_ store.SearchQuery) ([]store.SearchResult, error) {
	return nil, nil
}

func (m *mockSearchIndex) Upsert(r store.FactRecord) error {
	m.upserted = append(m.upserted, r)
	return nil
}

func (m *mockSearchIndex) Delete(path string) error {
	m.deleted = append(m.deleted, path)
	return nil
}

func (m *mockSearchIndex) Sync(_ store.GitReader) error {
	return nil
}

func (m *mockSearchIndex) GetLastCommit() (string, error) {
	return "", nil
}

// factContent builds a minimal knomit fact file for testing.
func factContent(title, body string) string {
	return "---\ndomain: [testing]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# " + title + "\n\n" + body + "\n"
}

func TestPruneStep(t *testing.T) {
	gs := newMockGitStore()
	gs.files["know/test/foo.md"] = factContent("Foo fact", "Foo is great.")
	gs.files["know/test/bar.md"] = factContent("Bar fact", "Bar is outdated.")
	gs.files["know/test/baz.md"] = factContent("Baz fact", "Baz needs confidence update.")

	// LLM returns: keep foo, forget bar, update baz with confidence=0.7
	mockResponse := `{
  "decisions": [
    { "path": "know/test/foo.md", "action": "keep" },
    { "path": "know/test/bar.md", "action": "forget" },
    { "path": "know/test/baz.md", "action": "update", "confidence": 0.7 }
  ],
  "merges": []
}`

	adapter := &mockLLM{response: mockResponse}
	idx := &mockSearchIndex{}
	recipe := Recipe{Name: "test-recipe", Steps: []RecipeStep{{Mode: "prune"}}}

	var events []ProgressEvent
	err := executePruneStep(context.Background(), gs, idx, adapter, recipe.Steps[0], recipe, func(e ProgressEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	// bar should be deleted
	barDeleted := false
	for _, d := range gs.deleted {
		if d == "know/test/bar.md" {
			barDeleted = true
		}
	}
	if !barDeleted {
		t.Errorf("expected know/test/bar.md to be deleted; deleted: %v", gs.deleted)
	}
	barIndexDeleted := false
	for _, d := range idx.deleted {
		if d == "know/test/bar.md" {
			barIndexDeleted = true
		}
	}
	if !barIndexDeleted {
		t.Errorf("expected know/test/bar.md to be removed from index; deleted: %v", idx.deleted)
	}

	// baz should have updated confidence in written content
	bazContent, ok := gs.written["know/test/baz.md"]
	if !ok {
		t.Fatal("expected know/test/baz.md to be rewritten with updated confidence")
	}
	if !strings.Contains(bazContent, "confidence: 0.7") {
		t.Errorf("baz.md content should contain 'confidence: 0.7', got:\n%s", bazContent)
	}

	// foo should be unchanged (not written, not deleted)
	fooWritten := false
	for p := range gs.written {
		if p == "know/test/foo.md" {
			fooWritten = true
		}
	}
	if fooWritten {
		t.Error("know/test/foo.md should not have been rewritten (keep action)")
	}
	fooDeleted := false
	for _, d := range gs.deleted {
		if d == "know/test/foo.md" {
			fooDeleted = true
		}
	}
	if fooDeleted {
		t.Error("know/test/foo.md should not have been deleted (keep action)")
	}

	// tag should be applied
	tagFound := false
	for _, tag := range gs.tags {
		if tag == "learn/synthesize-test-recipe-prune" {
			tagFound = true
		}
	}
	if !tagFound {
		t.Errorf("expected tag learn/synthesize-test-recipe-prune; tags: %v", gs.tags)
	}
}

func TestPruneStepWithMerge(t *testing.T) {
	gs := newMockGitStore()
	gs.files["know/test/a.md"] = factContent("A fact", "A says something.")
	gs.files["know/test/b.md"] = factContent("B fact", "B says the same thing.")

	mockResponse := `{
  "decisions": [],
  "merges": [
    {
      "paths": ["know/test/a.md", "know/test/b.md"],
      "merged": {
        "path": "know/test/ab-merged.md",
        "title": "A and B combined",
        "body": "Combined body.",
        "domain": ["testing"],
        "confidence": 0.85,
        "sources": 2,
        "entities": [],
        "refs": ["know/test/a.md", "know/test/b.md"]
      }
    }
  ]
}`

	adapter := &mockLLM{response: mockResponse}
	idx := &mockSearchIndex{}
	recipe := Recipe{Name: "merge-recipe", Steps: []RecipeStep{{Mode: "prune"}}}

	err := executePruneStep(context.Background(), gs, idx, adapter, recipe.Steps[0], recipe, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	// merged fact should be written
	if _, ok := gs.written["know/test/ab-merged.md"]; !ok {
		t.Error("expected merged fact know/test/ab-merged.md to be written")
	}

	// source facts should be deleted
	for _, src := range []string{"know/test/a.md", "know/test/b.md"} {
		found := false
		for _, d := range gs.deleted {
			if d == src {
				found = true
			}
		}
		if !found {
			t.Errorf("expected source %s to be deleted; deleted: %v", src, gs.deleted)
		}
	}
}

func TestChunkFacts(t *testing.T) {
	facts := []factForLLM{
		{File: "a.md", Title: "A", Body: "aaa"},
		{File: "b.md", Title: "B", Body: "bbb"},
		{File: "c.md", Title: "C", Body: "ccc"},
	}
	// Large limit: one chunk
	chunks := chunkFacts(facts, 1_000_000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk with large limit, got %d", len(chunks))
	}
	// Tiny limit: one fact per chunk
	chunks = chunkFacts(facts, 1)
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks with tiny limit, got %d", len(chunks))
	}
}

func TestExtractJSON(t *testing.T) {
	raw := "```json\n{\"hello\": \"world\"}\n```"
	got := extractJSON(raw)
	if got != `{"hello": "world"}` {
		t.Errorf("extractJSON: got %q", got)
	}
	plain := `{"hello": "world"}`
	if extractJSON(plain) != plain {
		t.Errorf("extractJSON plain: got %q", extractJSON(plain))
	}
}
