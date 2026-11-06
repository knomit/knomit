package chat

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/modelparams"
	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
)

// cosEmb embeds every query as (1, 0) and a tool's document at the angle that
// gives it the cosine in cos (a tool not in cos gets 0.1), so a test chooses
// the embedding ranker's ranking and how flat it is. Thresholds panics: a
// ranker that consults the recall cutoffs fails loudly.
type cosEmb struct {
	id  string
	cos map[string]float64
}

func (e *cosEmb) EmbedQuery(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }
func (e *cosEmb) EmbedDocument(_ context.Context, title, _ string) ([]float32, error) {
	c, ok := e.cos[title]
	if !ok {
		c = 0.1
	}
	return []float32{float32(c), float32(math.Sqrt(1 - c*c))}, nil
}
func (e *cosEmb) Dim() int   { return 2 }
func (e *cosEmb) ID() string { return e.id }
func (e *cosEmb) Thresholds() modelparams.Thresholds {
	panic("Thresholds consulted: tool ranking must not use a cosine cutoff")
}

func realTools(t *testing.T, h *harness) []llm.ToolDef {
	t.Helper()
	const sid = "chat-00000000000000000000000000000b10"
	s, _, tools, err := mcp.OpenInProcessSession(context.Background(), h.srv, h.repos, mcp.InProcessOptions{
		Principal: writer, SessionID: sid, Endpoint: "/api/v1/chats/" + sid,
		ClientName: "test-lister", ClientVersion: "x",
	})
	require.NoError(t, err)
	require.NoError(t, s.Close(context.Background()))
	require.NotEmpty(t, tools)
	return tools
}

func positionOf(rank []llm.ScoredTool, name string) int {
	for i, s := range rank {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// The live acceptance run's turn 3: "what do we know about how agents recover
// from tool errors?" against the real tool descriptions. The embedding scores
// were flat (0.17 to 0.19 over every tool), with knomit_review and knomit_skill
// on top; the fake embedder reproduces that band, with knomit_explain one place
// below review. Pure embedding ranks review and skill above explain (the old
// guess); the blend ranks explain above both, because the lexical ranker puts
// explain two places ahead of review and one ahead of skill and the embedding
// ranking, being flat, cannot outvote that.
//
// knomit_query is not the tool compared: the word overlap of its description
// with this message is nil (the lexical ranker puts it last), which is why the
// chat now keeps it in the window as a core tool instead of guessing it.
//
// SABOTAGE: adaptive.go newRanker, return the embedding ranker alone -> the
// blended assertions fail; drop the embedding ranker from the blend -> the
// "embedding alone puts review first" non-vacuity check is unaffected but the
// flat-band assertion on the order of review and explain fails.
func TestNewRanker_Turn3MessageRanksTheReadToolAboveReviewAndSkill(t *testing.T) {
	h := newHarness(t)
	tools := realTools(t, h)
	cos := map[string]float64{}
	for i, d := range tools {
		cos[d.Name] = 0.18 - 0.0005*float64(i) // a flat band, 0.17 to 0.18
	}
	cos["knomit_review"], cos["knomit_explain"], cos["knomit_skill"] = 0.1900, 0.1899, 0.1898
	emb := &cosEmb{id: t.Name(), cos: cos}
	const msg = "what do we know about how agents recover from tool errors?"

	old, err := mcp.NewEmbeddingRanker(emb, tools).Rank(context.Background(), msg)
	require.NoError(t, err)
	require.Less(t, positionOf(old, "knomit_review"), positionOf(old, "knomit_explain"), "the old guess: review above explain")
	require.Less(t, positionOf(old, "knomit_skill"), positionOf(old, "knomit_query"))

	r, _, primary := newRanker(emb, tools)
	require.Equal(t, rankerBlended, primary)
	got, err := r.Rank(context.Background(), msg)
	require.NoError(t, err)
	require.Len(t, got, len(tools), "every tool is ranked")
	require.Less(t, positionOf(got, "knomit_explain"), positionOf(got, "knomit_review"))
	require.Less(t, positionOf(got, "knomit_explain"), positionOf(got, "knomit_skill"))
}

// An embedder with no opinion (every tool the same score) adds nothing: the
// guess is the lexical ranker's, whatever order the tie would give by name.
//
// SABOTAGE: rank.go ranksOf, give tied tools consecutive ranks -> fails.
func TestNewRanker_AFlatEmbedderLeavesTheLexicalOrder(t *testing.T) {
	h := newHarness(t)
	tools := realTools(t, h)
	flat := &cosEmb{id: t.Name(), cos: map[string]float64{}}
	for _, d := range tools {
		flat.cos[d.Name] = 0.18
	}
	const msg = "what changed in the knowledge base since last week?"
	r, _, _ := newRanker(flat, tools)
	got, err := r.Rank(context.Background(), msg)
	require.NoError(t, err)
	lex, err := llm.NewLexicalRanker(tools).Rank(context.Background(), msg)
	require.NoError(t, err)
	var gotNames, lexNames []string
	for i := range got {
		gotNames = append(gotNames, got[i].Name)
		lexNames = append(lexNames, lex[i].Name)
	}
	require.Equal(t, lexNames[:5], gotNames[:5], "the lexical ranker's top five stand")
}
