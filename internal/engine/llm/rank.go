package llm

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"
)

// ScoredTool is one tool and how well the ranker thinks it fits a message.
// Scores are only comparable within one Rank call: they carry an order, not a
// meaning, and nothing may threshold them (invariant 9f110192: a similarity
// cutoff belongs to one model's distribution, and tool descriptions are another
// population).
type ScoredTool struct {
	Name  string
	Score float64
}

// ToolRanker guesses which tools a user message will need. Rank returns every
// tool it knows, best first, ties broken by name. An error means "no guess":
// the loop proceeds without a preload and the model loads what it needs.
type ToolRanker interface {
	Rank(ctx context.Context, message string) ([]ScoredTool, error)
}

// SortScored orders by score, best first, ties by name, so every ranker is
// deterministic.
func SortScored(s []ScoredTool) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		return s[i].Name < s[j].Name
	})
}

type lexicalRanker struct {
	names []string
	docs  []map[string]bool // distinct tokens of name and description, per tool
	idf   map[string]float64
}

// NewLexicalRanker ranks by TF-IDF over the tool documents: lowercase
// alphanumeric tokens, light plural stripping, the name split on '_' and
// counted once with the description. A tool's score is the sum of the idf of
// the query tokens its document holds. It has no tuned field weights, no
// network and no global state. It is the fallback when no embedder is wired or
// the embedder fails.
func NewLexicalRanker(tools []ToolDef) ToolRanker {
	r := &lexicalRanker{idf: map[string]float64{}}
	df := map[string]int{}
	for _, t := range tools {
		doc := map[string]bool{}
		for _, tok := range tokenize(t.Name + " " + t.Description) {
			doc[tok] = true
		}
		for tok := range doc {
			df[tok]++
		}
		r.names = append(r.names, t.Name)
		r.docs = append(r.docs, doc)
	}
	n := float64(len(tools))
	for tok, d := range df {
		r.idf[tok] = math.Log(1 + n/float64(d))
	}
	return r
}

func (r *lexicalRanker) Rank(_ context.Context, message string) ([]ScoredTool, error) {
	// The distinct query tokens, sorted: the sum below is taken in this order
	// for every tool and every call. Float addition is not associative, so a
	// sum in map order gave tied tools scores an ulp apart and let noise decide
	// the order of tools that should tie.
	seen := map[string]bool{}
	var query []string
	for _, tok := range tokenize(message) {
		if !seen[tok] {
			seen[tok] = true
			query = append(query, tok)
		}
	}
	sort.Strings(query)
	out := make([]ScoredTool, len(r.names))
	for i, name := range r.names {
		s := 0.0
		for _, tok := range query {
			if r.docs[i][tok] {
				s += r.idf[tok]
			}
		}
		out[i] = ScoredTool{Name: name, Score: s}
	}
	SortScored(out)
	return out, nil
}

// tokenize lowercases and splits on anything that is not a letter or digit
// (so '_' and '-' split names), then strips a plural ending.
func tokenize(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, t := range f {
		f[i] = singular(t)
	}
	return f
}

func singular(t string) string {
	switch {
	case len(t) > 4 && strings.HasSuffix(t, "ies"):
		return t[:len(t)-3] + "y"
	case len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss"):
		return t[:len(t)-1]
	}
	return t
}

// RRFConstant is k in reciprocal rank fusion: a tool's fused score is the sum,
// over the rankers, of 1/(k + its rank there). 60 is the value of the method's
// authors (Cormack, Clarke and Buettcher, SIGIR 2009), who found it robust
// across collections without tuning; it is kept because nothing here is better
// measured. It is a policy constant, not a measurement: with k that large a
// ranker's first place is worth only a little more than its tenth, so no single
// ranker's top pick can outvote a tool that both rankers place well, which is
// the point of blending a weak embedding of short descriptions (invariant
// 1d564e96) with word overlap.
const RRFConstant = 60

type blendedRanker struct{ rankers []ToolRanker }

// NewBlendedRanker fuses the rankings of rankers by reciprocal rank fusion
// (RRFConstant). Only ranks are combined, never scores: scores of different
// rankers are on different scales, and none may be thresholded (invariant
// 9f110192). Tools a ranker scores alike share its best rank among them
// (competition ranking), so a ranker with no opinion (every score equal) adds
// the same to every tool and the others' order stands; ties in the fused score
// go by name, so the result is deterministic. A ranker that fails is left out
// and the rest still guess; when every ranker fails the error is the last
// one's. Nil rankers are ignored, and a single ranker is returned as it is.
func NewBlendedRanker(rankers ...ToolRanker) ToolRanker {
	var rs []ToolRanker
	for _, r := range rankers {
		if r != nil {
			rs = append(rs, r)
		}
	}
	switch len(rs) {
	case 0:
		return nil
	case 1:
		return rs[0]
	}
	return blendedRanker{rs}
}

func (b blendedRanker) Rank(ctx context.Context, message string) ([]ScoredTool, error) {
	fused := map[string]float64{}
	var lastErr error
	answered := 0
	for _, r := range b.rankers {
		got, err := r.Rank(ctx, message)
		if err != nil {
			lastErr = err
			continue
		}
		answered++
		for name, rank := range ranksOf(got) {
			fused[name] += 1 / float64(RRFConstant+rank)
		}
	}
	if answered == 0 {
		return nil, lastErr
	}
	out := make([]ScoredTool, 0, len(fused))
	for name, s := range fused {
		out = append(out, ScoredTool{Name: name, Score: s})
	}
	SortScored(out)
	return out, nil
}

// ranksOf maps each tool to its 1-based rank in ranking (best first); tools
// with equal scores share the best rank of their group.
func ranksOf(ranking []ScoredTool) map[string]int {
	ranks := make(map[string]int, len(ranking))
	for i, s := range ranking {
		if i > 0 && s.Score == ranking[i-1].Score {
			ranks[s.Name] = ranks[ranking[i-1].Name]
			continue
		}
		ranks[s.Name] = i + 1
	}
	return ranks
}

type fallbackRanker struct{ primary, fallback ToolRanker }

// NewFallbackRanker asks primary and, when it errors (a slow or failed
// embedder), asks fallback instead, so a guess is made whenever any ranker can
// make one. The error returned is fallback's own. A nil primary is skipped.
func NewFallbackRanker(primary, fallback ToolRanker) ToolRanker {
	if primary == nil {
		return fallback
	}
	if fallback == nil {
		return primary
	}
	return fallbackRanker{primary, fallback}
}

func (f fallbackRanker) Rank(ctx context.Context, message string) ([]ScoredTool, error) {
	if got, err := f.primary.Rank(ctx, message); err == nil {
		return got, nil
	}
	return f.fallback.Rank(ctx, message)
}
