package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// testRepoUIDSeq backs nextTestRepoUID.
var testRepoUIDSeq atomic.Int64

// nextTestRepoUID returns a fresh, globally-unique uid for a test
// RepoInstance fixture. Binding.PinID() (repo:<uid>) is the MCP
// cursor-pinning identity (lenses RFC §7.3); two fixtures that are meant to
// be different bindings must never share a uid, or a foreign-binding
// rejection test would pass for the wrong reason (or not at all). Fixtures
// that don't otherwise care about uid (most of them) get one anyway — it's
// free and removes a whole class of accidental collision.
func nextTestRepoUID() string {
	return fmt.Sprintf("test-repo-%d", testRepoUIDSeq.Add(1))
}

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
	emb.EXPECT().EmbedQuery(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, text string) ([]float32, error) {
		return embed(text)
	}).AnyTimes()
	emb.EXPECT().EmbedDocument(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, title, body string) ([]float32, error) {
		return embed(title + " " + body)
	}).AnyTimes()
	emb.EXPECT().Dim().Return(768).AnyTimes()
	emb.EXPECT().ID().Return("len-stub").AnyTimes()
	emb.EXPECT().Thresholds().Return(params.Defaults()).AnyTimes()
	emb.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, titles, bodies []string) ([][]float32, error) {
		out := make([][]float32, len(titles))
		for i := range titles {
			out[i], _ = embed(titles[i] + " " + bodies[i])
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
		UID:          nextTestRepoUID(),
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

// principleLearnReq builds a single-fact learn request for a principle. Title
// and body are fixed so two requests dedup-match (cosine 1.0 with the
// length-based mock embedder); moment, confidence, and domain vary per call.
func principleLearnReq(moment string, confidence float64, domain []any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Test Principle",
				"body":       "designer authored this principle.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     domain,
				"confidence": confidence,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	return req
}

// mergedFactPath parses a successful learn result and returns its single
// committed file path.
func mergedFactPath(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	var parsed struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &parsed))
	require.Len(t, parsed.Commits, 1)
	return parsed.Commits[0].File
}

// newPrinciplesTestRepo opens an on-disk store wired with the real
// CodeOntology (which carries the principles validation rules) and a
// length-based mock embedder so dedup is deterministic.
func newPrinciplesTestRepo(t *testing.T) (*store.Service, context.Context, store.BatchEmbedder) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return svc, repos.WithRepoInstance(context.Background(), ri), emb
}

// TestLearnHandler_DedupMergePreservesKind regresses the bug where the
// dedup-merge branches built the merged fact via fact.NewFact but never
// copied Kind, leaving it empty (→ epistemic). For a principle (kind=
// pragmatic, type=policy) that made the merged fact an invalid epistemic/
// policy pair, so re-submitting the same principle failed must-be-pragmatic-
// policy. The merge must now succeed and the stored fact must keep its kind.
func TestLearnHandler_DedupMergePreservesKind(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)

	// First write: valid principle.
	r1, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write must succeed: %s", resultText(t, r1))

	// Second write: same canonical text → dedup matches (similarity 1.0),
	// higher confidence so the new fact wins the merge. With Kind copied, the
	// merged fact is a valid pragmatic/policy principle and the write succeeds.
	r2, err := LearnHandler(emb)(ctx, principleLearnReq("re-author", 0.9, []any{"global"}))
	require.NoError(t, err)
	require.False(t, r2.IsError,
		"re-authoring a principle must merge cleanly, not fail validation; got %s", resultText(t, r2))

	// The merged fact on disk must retain kind=pragmatic/type=policy and
	// reflect the merge (max confidence, summed sources).
	path := mergedFactPath(t, r2)
	res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
	require.NoError(t, err)
	merged, err := fact.ParseFact(path, res.Content)
	require.NoError(t, err)
	require.Equal(t, fact.Pragmatic, merged.Kind, "merge must preserve kind")
	require.Equal(t, fact.Policy, merged.Type, "merge must preserve type")
	require.Equal(t, 0.9, merged.Confidence, "merge keeps the higher confidence")
	require.Equal(t, 2, merged.Sources, "merge sums sources")
}

// TestLearnHandler_DedupMergeOneExistingFactAbsorbsOneIncoming regresses the
// collision the handler's duplicate-path guard cannot see. That guard runs over
// the freshly-minted paths; applyDedupMerge retargets paths[i] onto an existing
// fact's path AFTERWARDS, so two inputs in one category that both match the
// SAME existing fact used to land on one path — one file written, one body
// silently gone, two commit entries emitted, and a summary asserting both were
// written. An existing fact now absorbs at most one incoming fact per call; the
// second is written at its own path, so nothing the caller sent disappears and
// the reported count is a count that happened.
func TestLearnHandler_DedupMergeOneExistingFactAbsorbsOneIncoming(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)

	r1, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write must succeed: %s", resultText(t, r1))
	seedPath := mergedFactPath(t, r1)

	// Two facts in ONE call, both canonically identical to the seed, so both
	// dedup-search back to the same existing fact.
	req := principleLearnReq("collide", 0.9, []any{"global"})
	facts := req.Params.Arguments.(map[string]any)["facts"].([]any)
	req.Params.Arguments.(map[string]any)["facts"] = []any{facts[0], facts[0]}

	r2, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.False(t, r2.IsError, "colliding dedup matches must not fail the call: %s", resultText(t, r2))

	var parsed struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, r2)), &parsed))
	require.Len(t, parsed.Commits, 2, "both inputs are accounted for")

	distinct := map[string]bool{}
	for _, c := range parsed.Commits {
		distinct[c.File] = true
	}
	require.Len(t, distinct, 2,
		"two inputs collapsed onto one file while the response reported two: %v", parsed.Commits)
	require.Contains(t, parsed.Summary, "2 facts",
		"the summary must count what was actually written; got %q", parsed.Summary)

	// One input merged into the seed; the other kept its own freshly-minted
	// path. Every file the response names must actually carry a body.
	require.True(t, distinct[seedPath], "one input must still have merged into the existing fact")
	for file := range distinct {
		res, rerr := svc.Facts().ReadFact(context.Background(), "agent/test", file, nil)
		require.NoError(t, rerr, "response named %s but it is not on disk", file)
		require.NotEmpty(t, res.Content, "%s was written empty", file)
	}
}

// TestLearnHandler_DedupMergeReValidates regresses the re-validate step: a
// merge can produce a rule violation even when both inputs were individually
// valid. Here a [global] principle and an identically-worded [store]-scoped
// principle dedup-merge; the unioned domain becomes [global, store], which
// violates domain-mutually-exclusive. Without re-validation this would only
// surface later as an opaque serialize error.
func TestLearnHandler_DedupMergeReValidates(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)

	// First write: valid global principle.
	r1, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write must succeed: %s", resultText(t, r1))

	// Second write: same canonical text (so it dedup-merges) but scoped to an
	// area instead of global. Individually valid, but the merge unions the
	// domains into [global, store] → violates domain-mutually-exclusive.
	r2, err := LearnHandler(emb)(ctx, principleLearnReq("merge-conflict", 0.9, []any{"store"}))
	require.NoError(t, err)
	require.True(t, r2.IsError, "expected dedup-merge to surface a rule violation; got success")
	text := resultText(t, r2)
	require.Contains(t, text, "dedup-merge",
		"error must identify the dedup-merge stage; got %q", text)
	require.Contains(t, text, "domain-mutually-exclusive",
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
