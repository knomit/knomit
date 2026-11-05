package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func rankTools() []ToolDef {
	return []ToolDef{
		{Name: "knomit_query", Description: "Search the knowledge base for facts."},
		{Name: "knomit_bind", Description: "Attach this session to a repository handle."},
		{Name: "knomit_review", Description: "Run maintenance over stored entries."},
	}
}

func names(s []ScoredTool) []string {
	var out []string
	for _, t := range s {
		out = append(out, t.Name)
	}
	return out
}

func TestLexical_RanksTheNamedToolFirst(t *testing.T) {
	got, err := NewLexicalRanker(rankTools()).Rank(context.Background(), "search the facts about retries")
	require.NoError(t, err)
	require.Equal(t, "knomit_query", got[0].Name)
	require.Greater(t, got[0].Score, got[1].Score)
	require.Len(t, got, 3, "every tool is ranked")
}

func TestLexical_NameTokensCount(t *testing.T) {
	got, err := NewLexicalRanker(rankTools()).Rank(context.Background(), "bind")
	require.NoError(t, err)
	require.Equal(t, "knomit_bind", got[0].Name, "the description never says bind; the name does")
	require.Greater(t, got[0].Score, 0.0)
	require.Equal(t, 0.0, got[1].Score)
}

func TestLexical_PluralStripping(t *testing.T) {
	got, err := NewLexicalRanker(rankTools()).Rank(context.Background(), "entry")
	require.NoError(t, err)
	require.Equal(t, "knomit_review", got[0].Name, `"entries" in the description matches "entry"`)
	require.Greater(t, got[0].Score, 0.0)
	got, err = NewLexicalRanker(rankTools()).Rank(context.Background(), "repositories")
	require.NoError(t, err)
	require.Equal(t, "knomit_bind", got[0].Name, `"repository" matches "repositories"`)
	require.Greater(t, got[0].Score, 0.0)
}

func TestLexical_EmptyMessageReturnsAllWithEqualScores(t *testing.T) {
	got, err := NewLexicalRanker(rankTools()).Rank(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, got, 3)
	for _, s := range got {
		require.Equal(t, 0.0, s.Score)
	}
	require.Equal(t, []string{"knomit_bind", "knomit_query", "knomit_review"}, names(got))
}

func TestLexical_DeterministicOrderOnTies(t *testing.T) {
	tools := []ToolDef{{Name: "zeta", Description: "same words"}, {Name: "alpha", Description: "same words"}, {Name: "mid", Description: "same words"}}
	for i := 0; i < 5; i++ {
		got, err := NewLexicalRanker(tools).Rank(context.Background(), "same words")
		require.NoError(t, err)
		require.Equal(t, []string{"alpha", "mid", "zeta"}, names(got))
	}
}

type stubRanker struct {
	out   []ScoredTool
	err   error
	calls int
}

func (s *stubRanker) Rank(context.Context, string) ([]ScoredTool, error) {
	s.calls++
	return s.out, s.err
}

func TestFallbackRanker_UsesLexicalWhenThePrimaryErrors(t *testing.T) {
	primary := &stubRanker{err: context.DeadlineExceeded}
	lex := NewLexicalRanker(rankTools())
	got, err := NewFallbackRanker(primary, lex).Rank(context.Background(), "bind")
	require.NoError(t, err)
	require.Equal(t, 1, primary.calls, "the primary was tried first")
	require.Equal(t, "knomit_bind", got[0].Name, "the answer is the lexical ranker's")
}

func TestFallbackRanker_PrimaryAnswerWinsAndFallbackIsNotAsked(t *testing.T) {
	primary := &stubRanker{out: []ScoredTool{{Name: "knomit_review", Score: 1}}}
	fb := &stubRanker{out: []ScoredTool{{Name: "knomit_bind", Score: 1}}}
	got, err := NewFallbackRanker(primary, fb).Rank(context.Background(), "bind")
	require.NoError(t, err)
	require.Equal(t, "knomit_review", got[0].Name)
	require.Zero(t, fb.calls)
}

func TestFallbackRanker_NilPrimaryIsTheFallback(t *testing.T) {
	fb := &stubRanker{out: []ScoredTool{{Name: "x", Score: 1}}}
	require.Same(t, fb, NewFallbackRanker(nil, fb))
}
