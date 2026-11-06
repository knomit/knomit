package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Two tools whose documents hold the same query tokens must get the same score,
// bit for bit, in every call: the sum of idf over the query tokens is taken in a
// fixed order. A sum over a map walks its keys in a random order, and float
// addition is not associative, so tied tools differed by an ulp and the
// tie-break by name was decided by noise (the flaky
// TestNewRanker_AFlatEmbedderLeavesTheLexicalOrder in the chat package).
//
// SABOTAGE: rank.go lexicalRanker.Rank, range over the query map instead of the
// ordered tokens -> the scores of twin_a and twin_b differ in some call.
func TestLexicalRanker_TiedToolsScoreIdenticallyInEveryCall(t *testing.T) {
	// Twenty query words with distinct document frequencies, so distinct idf.
	var words []string
	var tools []ToolDef
	for i := 0; i < 20; i++ {
		w := fmt.Sprintf("word%c", 'a'+i)
		words = append(words, w)
		for j := 0; j < i; j++ { // word i appears in i filler tools
			tools = append(tools, ToolDef{Name: fmt.Sprintf("filler_%d_%d", i, j), Description: w})
		}
	}
	doc := strings.Join(words, " ")
	tools = append(tools, ToolDef{Name: "twin_a", Description: doc}, ToolDef{Name: "twin_b", Description: doc})
	r := NewLexicalRanker(tools)
	msg := strings.Join(words, " ")

	var first []ScoredTool
	for i := 0; i < 300; i++ {
		got, err := r.Rank(context.Background(), msg)
		require.NoError(t, err)
		score := map[string]float64{}
		for _, s := range got {
			score[s.Name] = s.Score
		}
		require.Equal(t, score["twin_a"], score["twin_b"], "call %d", i)
		if first == nil {
			first = got
		}
		require.Equal(t, first, got, "call %d: the ranking is the same every time", i)
	}
}
