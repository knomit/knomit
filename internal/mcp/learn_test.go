package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// newLenEmbedder returns a mock BatchEmbedder whose 768-dim vectors depend
// only on text length, so any two equal-length strings embed identically
// (cosine 1.0). That determinism is what lets the dedup path be driven
// predictably. Both methods accept any number of calls.
func newLenEmbedder(t *testing.T) *MockBatchEmbedder {
	t.Helper()
	emb := NewMockBatchEmbedder(gomock.NewController(t))
	embed := func(text string) ([]float32, error) {
		out := make([]float32, 768)
		for i := range out {
			out[i] = float32((len(text)*31+i)%256) / 256.0
		}
		return out, nil
	}
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(embed).AnyTimes()
	emb.EXPECT().EmbedBatch(gomock.Any()).DoAndReturn(func(texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i], _ = embed(txt)
		}
		return out, nil
	}).AnyTimes()
	return emb
}

// principlesOntologyYAML is a tiny ontology with one validation rule that
// enforces 'designer' must be present in fact.entities. It is reused by the
// two validation tests below.
const principlesOntologyYAML = `id: t
name: T
topics:
  principles:
    description: x
    validations:
      - name: must-have-designer
        message: "principles must be authored via /knomit-principle"
        rule: "fact.entities.includes('designer')"
    children:
      mission:
        description: x
`

// newLearnTestRepo opens a fresh on-disk store, initialises the agent branch,
// and returns a RepoInstance wired with the given ontology. The OntologyRoot
// is "kb" — matching the handler's expectation that BuildFactPath uses that
// prefix.
func newLearnTestRepo(t *testing.T, ontology *fact.Ontology) *repos.RepoInstance {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     ontology,
		OntologyRoot: "kb",
	})
}

// resultText extracts the first TextContent string from an MCP result.
func resultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("result has no TextContent: %+v", result.Content)
	return ""
}

// TestLearnHandler_RejectsFailingValidation seeds a repo with an ontology
// that requires entities to include 'designer', then attempts to write a
// fact without that entity. The handler must reject the write with an
// error referencing the rule name AND the rule's message.
func TestLearnHandler_RejectsFailingValidation(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)

	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "reject-test",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Bad Principle",
				"body":       "missing designer entity.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{},
				"refs":       []any{},
			},
		},
	}

	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "expected validation failure to surface as IsError result")

	text := resultText(t, result)
	require.Contains(t, text, "must-have-designer",
		"error must reference the rule name; got %q", text)
	require.Contains(t, text, "/knomit-principle",
		"error must include the rule's message; got %q", text)
}

// TestLearnHandler_DedupMergeReValidates regresses the gap where a dedup
// merge could produce a fact that violates ontology rules. Without the
// re-validate step, a violation only surfaces later as an opaque
// serialize/write error.
//
// Setup uses the real CodeOntology principles topic. First write is a
// valid principle. Second write uses the same title+body (so dedup
// similarity is 1.0 with the stub embedder, hitting the merge branch)
// and is itself a valid principle — but the merge logic (see learn.go,
// "New fact wins" branch) does not copy Kind onto merged, so the merged
// fact violates must-be-pragmatic-policy. The handler must surface that
// failure with the rule name, attributed to the dedup-merge stage.
//
// Note: the Kind-not-copied behavior in the merge branch is itself a
// pre-existing latent bug. This test asserts the surfacing behavior; the
// underlying merge bug is out of scope for this follow-up.
func TestLearnHandler_DedupMergeReValidates(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	ctx := repos.WithRepoInstance(context.Background(), ri)

	const title = "Test Principle"
	const body = "designer authored this principle."

	// First write: valid principle.
	var first mcpgo.CallToolRequest
	first.Params.Arguments = map[string]any{
		"moment_name": "seed",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      title,
				"body":       body,
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{"global"},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	r1, err := LearnHandler(emb)(ctx, first)
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write must succeed: %s", resultText(t, r1))

	// Second write: same canonical text → dedup matches with similarity 1.0,
	// new-fact-wins branch (confidence 0.9 > 0.8). The merge produces a
	// fact whose Kind is unset (separate latent bug), so the merged fact
	// violates must-be-pragmatic-policy. Pre-merge ValidateFact passed.
	var second mcpgo.CallToolRequest
	second.Params.Arguments = map[string]any{
		"moment_name": "merge-conflict",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      title,
				"body":       body,
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{"global"},
				"confidence": 0.9, // higher → new fact wins the merge
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	r2, err := LearnHandler(emb)(ctx, second)
	require.NoError(t, err)
	require.True(t, r2.IsError, "expected dedup-merge to surface a rule violation; got success")
	text := resultText(t, r2)
	require.Contains(t, text, "dedup-merge",
		"error must identify the dedup-merge stage; got %q", text)
	require.Contains(t, text, "must-be-pragmatic-policy",
		"error must reference the failing rule name; got %q", text)
}

// TestLearnHandler_AcceptsValidFact seeds the same ontology and writes a
// fact that satisfies the rule. The write must succeed (no error).
func TestLearnHandler_AcceptsValidFact(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)

	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "accept-test",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Good Principle",
				"body":       "designer authored this.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}

	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", resultText(t, result))
	}

	// Sanity: response must contain at least one committed file path under
	// the expected category directory.
	text := resultText(t, result)
	require.Contains(t, text, "kb/principles/mission/foo/",
		"response should include the written fact path; got %q", text)
}
