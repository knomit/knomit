package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/modelparams"
	"knomit/internal/engine/llm"
)

// rankEmbedder is a deterministic Embedder: vectors come from tables keyed by
// text, and every call is recorded. Thresholds panics, so a ranker that
// consults the recall cutoffs fails loudly.
type rankEmbedder struct {
	id      string
	docs    map[string][]float32 // by tool name
	queries map[string][]float32 // by message
	docCall []string             // "name|description" per EmbedDocument
	qCall   []string
	err     error
}

func (e *rankEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	e.qCall = append(e.qCall, text)
	if e.err != nil {
		return nil, e.err
	}
	return e.queries[text], nil
}

func (e *rankEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	e.docCall = append(e.docCall, title+"|"+body)
	return e.docs[title], nil
}
func (e *rankEmbedder) Dim() int   { return 3 }
func (e *rankEmbedder) ID() string { return e.id }
func (e *rankEmbedder) Thresholds() modelparams.Thresholds {
	panic("Thresholds consulted: tool ranking must not use a cosine cutoff")
}

func rankToolset() []llm.ToolDef {
	return []llm.ToolDef{
		{Name: "tool_a", Description: "alpha apples"},
		{Name: "tool_b", Description: "beta bananas"},
		{Name: "tool_c", Description: "gamma cherries"},
	}
}

// rankEmbedderSeq numbers fakes so each has its own embedder id: the tool
// vector cache is process-wide and keyed by it, so an id shared across -count
// runs of one test would hand a later run the earlier run's vectors and its
// fake would record no EmbedDocument calls.
var rankEmbedderSeq atomic.Int64

func newRankEmbedder(t *testing.T) *rankEmbedder {
	return &rankEmbedder{
		id: fmt.Sprintf("%s#%d", t.Name(), rankEmbedderSeq.Add(1)),
		docs: map[string][]float32{
			"tool_a": {1, 0, 0},
			"tool_b": {0, 1, 0},
			"tool_c": {0, 0, 1},
		},
		queries: map[string][]float32{},
	}
}

func scoredNames(s []llm.ScoredTool) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Name)
	}
	return out
}

func TestEmbeddingRanker_OrdersByCosine(t *testing.T) {
	e := newRankEmbedder(t)
	// The message shares no word with tool_c's description, yet its vector is
	// closest to tool_c: the order is the embedder's, not the lexical one.
	e.queries["apples please"] = []float32{0.1, 0.2, 0.9}
	got, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "apples please")
	require.NoError(t, err)
	require.Equal(t, []string{"tool_c", "tool_b", "tool_a"}, scoredNames(got))
	lex, _ := llm.NewLexicalRanker(rankToolset()).Rank(context.Background(), "apples please")
	require.Equal(t, "tool_a", lex[0].Name, "control: the lexical ranker picks the other tool")
	require.InDelta(t, 0.9/math.Sqrt(0.86), got[0].Score, 1e-6)
}

func TestEmbeddingRanker_EmbedsEachToolOncePerToolset(t *testing.T) {
	e := newRankEmbedder(t)
	e.queries["m"] = []float32{1, 1, 1}
	_, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "m")
	require.NoError(t, err)
	require.Len(t, e.docCall, 3, "first ranker embeds every tool")

	_, err = NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "m")
	require.NoError(t, err)
	require.Len(t, e.docCall, 3, "a second ranker over the same tools embeds none")

	changed := rankToolset()
	changed[1].Description = "beta bananas, revised"
	_, err = NewEmbeddingRanker(e, changed).Rank(context.Background(), "m")
	require.NoError(t, err)
	require.Equal(t, []string{"tool_a|alpha apples", "tool_b|beta bananas", "tool_c|gamma cherries", "tool_b|beta bananas, revised"}, e.docCall, "only the changed description is embedded again")
}

func TestEmbeddingRanker_UsesQueryRoleForTheMessage(t *testing.T) {
	e := newRankEmbedder(t)
	e.queries["the message"] = []float32{1, 0, 0}
	_, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "the message")
	require.NoError(t, err)
	require.Equal(t, []string{"the message"}, e.qCall, "the message is a query")
	require.ElementsMatch(t, []string{"tool_a|alpha apples", "tool_b|beta bananas", "tool_c|gamma cherries"}, e.docCall, "tools are documents (name, description)")
}

func TestEmbeddingRanker_NoAbsoluteCutoff(t *testing.T) {
	e := newRankEmbedder(t)
	e.queries["m"] = []float32{0.001, 0.0002, 0.0003} // tiny norm: cosine is scale-free, the order is by direction
	got, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "m")
	require.NoError(t, err)
	require.Len(t, got, 3, "every tool is ranked however low the best cosine is; Thresholds() would have panicked")
	require.Equal(t, "tool_a", got[0].Name)
	// Negative cosines are ranked too.
	e.queries["neg"] = []float32{-1, -2, -3}
	got, err = NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "neg")
	require.NoError(t, err)
	require.Equal(t, []string{"tool_a", "tool_b", "tool_c"}, scoredNames(got), "least negative first")
	require.Less(t, got[0].Score, 0.0)
}

func TestEmbeddingRanker_ErrorPropagates(t *testing.T) {
	e := newRankEmbedder(t)
	boom := errors.New("onnx session closed")
	e.err = boom
	got, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "m")
	require.Nil(t, got)
	require.ErrorIs(t, err, boom)
}

// WarmToolVectors embeds the toolset once, ahead of the first Rank, so a chat's
// first turn does not embed every tool on the request path; the Rank that
// follows embeds none.
//
// SABOTAGE: make WarmToolVectors return without embedding -> the Rank after it
// embeds three tools and this fails.
func TestWarmToolVectors_EmbedsOnceAndRankEmbedsNone(t *testing.T) {
	e := newRankEmbedder(t)
	e.queries["m"] = []float32{1, 1, 1}
	require.NoError(t, WarmToolVectors(context.Background(), e, rankToolset()))
	require.Len(t, e.docCall, 3, "warming embeds every tool")
	require.Empty(t, e.qCall, "warming embeds no query")

	_, err := NewEmbeddingRanker(e, rankToolset()).Rank(context.Background(), "m")
	require.NoError(t, err)
	require.Len(t, e.docCall, 3, "the first Rank embedded no tool")
	require.Equal(t, []string{"m"}, e.qCall)

	// Warming again costs nothing.
	require.NoError(t, WarmToolVectors(context.Background(), e, rankToolset()))
	require.Len(t, e.docCall, 3)
}

func TestWarmToolVectors_StopsWhenTheChatEnds(t *testing.T) {
	e := newRankEmbedder(t)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, WarmToolVectors(ended, e, rankToolset()))
	require.Empty(t, e.docCall)
}
