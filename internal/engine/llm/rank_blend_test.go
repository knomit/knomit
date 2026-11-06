package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func scored(pairs ...any) []ScoredTool {
	var out []ScoredTool
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, ScoredTool{Name: pairs[i].(string), Score: pairs[i+1].(float64)})
	}
	SortScored(out)
	return out
}

func blendOrder(t *testing.T, rs ...ToolRanker) []string {
	t.Helper()
	got, err := NewBlendedRanker(rs...).Rank(context.Background(), "m")
	require.NoError(t, err)
	return names(got)
}

// A tool both rankers like beats a tool only one of them loves.
//
// SABOTAGE: rank.go blendedRanker.Rank, fuse with `1` instead of
// `1/(RRFConstant+rank)` -> every tool ties and the order is by name: fails.
func TestBlend_ABothRankersLikeBeatsAnotherOneRankerLoves(t *testing.T) {
	a := &stubRanker{out: scored("x", 9.0, "y", 8.0, "z", 1.0)}
	b := &stubRanker{out: scored("y", 5.0, "z", 4.0, "x", 1.0)}
	require.Equal(t, []string{"y", "x", "z"}, blendOrder(t, a, b))
}

// Only ranks are mixed, never scores: multiplying one ranker's scores by a
// million, or shifting them, changes nothing.
//
// SABOTAGE: rank.go, add the raw score into the fused value -> fails.
func TestBlend_UsesRanksNotScores(t *testing.T) {
	small := scored("x", 0.19, "y", 0.18, "z", 0.17)
	big := scored("y", 9e6, "x", 5e6, "z", 1e6)
	want := blendOrder(t, &stubRanker{out: small}, &stubRanker{out: big})
	require.Equal(t, []string{"x", "y", "z"}, want, "x and y swap places between the rankers, so they tie and go by name; raw scores would put y first")
	rescaled := scored("x", 0.19*1e6+7, "y", 0.18*1e6+7, "z", 0.17*1e6+7)
	require.Equal(t, want, blendOrder(t, &stubRanker{out: rescaled}, &stubRanker{out: big}))
}

// Tied scores carry no evidence: a ranker that scores every tool alike adds the
// same to each, so the other ranker's order stands.
//
// SABOTAGE: rank.go ranksOf, give tied tools consecutive ranks (by name) ->
// the flat ranker's alphabetical order leaks in and this fails.
func TestBlend_AFlatRankerAddsNothing(t *testing.T) {
	flat := &stubRanker{out: scored("a", 0.0, "b", 0.0, "c", 0.0, "d", 0.0)}
	informed := &stubRanker{out: scored("d", 3.0, "c", 2.0, "b", 1.0, "a", 0.5)}
	require.Equal(t, []string{"d", "c", "b", "a"}, blendOrder(t, flat, informed))
	require.Equal(t, []string{"d", "c", "b", "a"}, blendOrder(t, informed, flat))
}

// A ranker that fails is left out and the others still guess; when every ranker
// fails there is no guess and the error is returned.
//
// SABOTAGE: rank.go, return the first error instead of skipping it -> fails.
func TestBlend_FailedRankerIsSkipped(t *testing.T) {
	good := &stubRanker{out: scored("x", 2.0, "y", 1.0)}
	bad := &stubRanker{err: errors.New("embedder down")}
	require.Equal(t, []string{"x", "y"}, blendOrder(t, bad, good))
	require.Equal(t, 1, bad.calls)

	_, err := NewBlendedRanker(bad, &stubRanker{err: errors.New("lexical down")}).Rank(context.Background(), "m")
	require.Error(t, err)
}

func TestBlend_NilRankersAreIgnoredAndDeterministic(t *testing.T) {
	good := &stubRanker{out: scored("b", 1.0, "a", 1.0)}
	for i := 0; i < 5; i++ {
		require.Equal(t, []string{"a", "b"}, blendOrder(t, nil, good), "ties go by name")
	}
	require.Same(t, good, NewBlendedRanker(nil, good))
}

// RRFConstant is the published value; the test pins it so a change is a
// decision made here.
func TestBlend_RRFConstant(t *testing.T) {
	require.Equal(t, 60, RRFConstant)
}
