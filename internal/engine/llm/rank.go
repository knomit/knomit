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
	query := map[string]bool{}
	for _, tok := range tokenize(message) {
		query[tok] = true
	}
	out := make([]ScoredTool, len(r.names))
	for i, name := range r.names {
		s := 0.0
		for tok := range query {
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
