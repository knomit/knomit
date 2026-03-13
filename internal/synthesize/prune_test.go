package synthesize

import (
	"context"
	"encoding/json"
	"math"
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
	err := executePruneStep(context.Background(), gs, idx, nil, adapter, recipe.Steps[0], recipe, func(e ProgressEvent) {
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

	err := executePruneStep(context.Background(), gs, idx, nil, adapter, recipe.Steps[0], recipe, func(ProgressEvent) {})
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

func TestParsePruneResponseMarkdownWrapped(t *testing.T) {
	// LLMs sometimes wrap their JSON in markdown code fences.
	wrapped := "```json\n" + `{
  "decisions": [
    { "path": "know/x.md", "action": "keep" }
  ],
  "merges": []
}` + "\n```"

	result, err := parsePruneResponse(wrapped)
	if err != nil {
		t.Fatalf("parsePruneResponse with markdown wrapping: %v", err)
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(result.Decisions))
	}
	if result.Decisions[0].Path != "know/x.md" {
		t.Errorf("expected path 'know/x.md', got %q", result.Decisions[0].Path)
	}
	if result.Decisions[0].Action != "keep" {
		t.Errorf("expected action 'keep', got %q", result.Decisions[0].Action)
	}
	if len(result.Merges) != 0 {
		t.Errorf("expected no merges, got %d", len(result.Merges))
	}
}

func TestParseDistillResponseMarkdownWrapped(t *testing.T) {
	wrapped := "```json\n" + `{
  "synthesize": [
    {
      "path": "know/synth.md",
      "title": "Synthesized",
      "body": "Combined insight.",
      "domain": ["testing"],
      "confidence": 0.85,
      "entities": [],
      "refs": ["know/a.md"]
    }
  ],
  "forget": ["know/a.md"]
}` + "\n```"

	result, err := parseDistillResponse(wrapped)
	if err != nil {
		t.Fatalf("parseDistillResponse with markdown wrapping: %v", err)
	}
	if len(result.Synthesize) != 1 {
		t.Fatalf("expected 1 synthesized fact, got %d", len(result.Synthesize))
	}
	if result.Synthesize[0].Path != "know/synth.md" {
		t.Errorf("expected path 'know/synth.md', got %q", result.Synthesize[0].Path)
	}
	if result.Synthesize[0].Title != "Synthesized" {
		t.Errorf("expected title 'Synthesized', got %q", result.Synthesize[0].Title)
	}
	if len(result.Forget) != 1 || result.Forget[0] != "know/a.md" {
		t.Errorf("expected forget=[know/a.md], got %v", result.Forget)
	}
}

func TestChunkFactsExceedsBudget(t *testing.T) {
	// Build many facts so that the total JSON exceeds a small budget,
	// forcing the chunker to split across multiple chunks.
	facts := make([]factForLLM, 10)
	for i := range facts {
		facts[i] = factForLLM{
			File:  "know/fact.md",
			Title: "A moderately long title that takes up space",
			Body:  "A moderately long body that contributes to the chunk budget.",
		}
	}

	// Measure a single fact's size.
	import_json_b, _ := json.Marshal(facts[0])
	singleSize := len(import_json_b)

	// Budget that fits exactly 3 facts — expect ceil(10/3) = 4 chunks.
	budget := singleSize * 3
	chunks := chunkFacts(facts, budget)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks when budget is tight, got %d chunk(s)", len(chunks))
	}
	// Every chunk must be non-empty.
	for i, ch := range chunks {
		if len(ch) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
	}
	// Total facts across all chunks must equal original count.
	total := 0
	for _, ch := range chunks {
		total += len(ch)
	}
	if total != len(facts) {
		t.Errorf("expected %d total facts across chunks, got %d", len(facts), total)
	}
}

// capturingLLM records prompts sent to the LLM and returns a fixed response.
type capturingLLM struct {
	response string
	prompts  []string
}

func (m *capturingLLM) Complete(_ context.Context, _ string, msgs []llm.Message, _ func(string)) (string, error) {
	for _, msg := range msgs {
		m.prompts = append(m.prompts, msg.Content)
	}
	return m.response, nil
}

// mockPruneIndex wraps mockSearchIndex and adds PairwiseDistances for clustering tests.
type mockPruneIndex struct {
	mockSearchIndex
	embeddings map[string][]float32
}

// PairwiseDistances computes cosine distances between the given paths using stored embeddings.
func (m *mockPruneIndex) PairwiseDistances(paths []string) ([]string, [][]float64, error) {
	// Filter to paths with embeddings.
	var filtered []string
	for _, p := range paths {
		if _, ok := m.embeddings[p]; ok {
			filtered = append(filtered, p)
		}
	}
	n := len(filtered)
	if n == 0 {
		return nil, nil, nil
	}

	dist := make([][]float64, n)
	for i := range dist {
		dist[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			d := testCosineDistance(m.embeddings[filtered[i]], m.embeddings[filtered[j]])
			dist[i][j] = d
			dist[j][i] = d
		}
	}
	return filtered, dist, nil
}

// testCosineDistance computes 1 - cosine_similarity for float32 slices.
func testCosineDistance(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 1.0
	}
	sim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	return 1.0 - sim
}

// stubEmbedder is a no-op embedder that satisfies the Embedder interface.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ string) ([]float32, error) {
	return []float32{0, 0, 0, 0}, nil
}

// TestPruneStepClustersBeforeLLM verifies that when embeddings are available,
// the prune step clusters facts and only sends clustered groups to the LLM,
// rather than sending all facts at once.
func TestPruneStepClustersBeforeLLM(t *testing.T) {
	gs := newMockGitStore()

	// Create two tight clusters of 5 facts each with well-separated embeddings.
	// Uses cosine distance, so direction matters — cluster A points along dim 0,
	// cluster B points along dim 7. Small perturbations in other dims for variety.
	clusterAFiles := []string{
		"know/cluster-a/a1.md", "know/cluster-a/a2.md", "know/cluster-a/a3.md",
		"know/cluster-a/a4.md", "know/cluster-a/a5.md",
	}
	clusterBFiles := []string{
		"know/cluster-b/b1.md", "know/cluster-b/b2.md", "know/cluster-b/b3.md",
		"know/cluster-b/b4.md", "know/cluster-b/b5.md",
	}
	noiseFile := "know/noise/lone.md"

	allFiles := make([]string, 0, len(clusterAFiles)+len(clusterBFiles)+1)
	allFiles = append(allFiles, clusterAFiles...)
	allFiles = append(allFiles, clusterBFiles...)
	allFiles = append(allFiles, noiseFile)

	for _, f := range allFiles {
		gs.files[f] = factContent("Fact "+f, "Body of "+f)
	}

	embeddingMap := map[string][]float32{}
	// Cluster A: dominant component in dim 0, small noise in other dims.
	for i, f := range clusterAFiles {
		v := make([]float32, 16)
		v[0] = 10.0
		v[1] = float32(i) * 0.1 // small perturbation
		embeddingMap[f] = v
	}
	// Cluster B: dominant component in dim 7, small noise in other dims.
	for i, f := range clusterBFiles {
		v := make([]float32, 16)
		v[7] = 10.0
		v[8] = float32(i) * 0.1
		embeddingMap[f] = v
	}
	// Noise: equal weight across multiple dims — equidistant from both clusters.
	embeddingMap[noiseFile] = []float32{1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1}

	idx := &mockPruneIndex{embeddings: embeddingMap}
	adapter := &capturingLLM{response: `{"decisions":[],"merges":[]}`}
	recipe := Recipe{Name: "cluster-test", Steps: []RecipeStep{{Mode: "prune", MinClusterSize: 3}}}

	err := executePruneStep(context.Background(), gs, idx, stubEmbedder{}, adapter, recipe.Steps[0], recipe, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	// The noise fact (lone.md) should NOT appear in any LLM prompt.
	for i, prompt := range adapter.prompts {
		if strings.Contains(prompt, noiseFile) {
			t.Errorf("prompt %d contains noise fact %s — it should have been skipped as unclustered", i, noiseFile)
		}
	}

	// With 2 clusters we expect exactly 2 LLM calls, not 1 call with all 11 facts.
	if len(adapter.prompts) != 2 {
		t.Errorf("expected 2 LLM calls (one per cluster), got %d", len(adapter.prompts))
	}

	// Each prompt should contain only facts from its cluster, not from both.
	for _, prompt := range adapter.prompts {
		hasA := strings.Contains(prompt, "know/cluster-a/")
		hasB := strings.Contains(prompt, "know/cluster-b/")
		if hasA && hasB {
			t.Error("a single LLM prompt contains facts from both clusters — they should be separated")
		}
	}
}
