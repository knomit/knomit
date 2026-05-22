package detect

import (
	"fmt"

	"knomit/internal/store"
)

// Block is one entry in a transcript window passed to /detect.
type Block struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Signal is one (intent, score) pair on a block.
type Signal struct {
	Intent string  `json:"intent"`
	Score  float64 `json:"score"`
}

// SimilarFact identifies a knomit fact close to the block embedding.
type SimilarFact struct {
	Path       string  `json:"path"`
	Similarity float64 `json:"similarity"`
}

// BlockResult holds the scoring output for a single block.
type BlockResult struct {
	Index        int           `json:"index"`
	Signals      []Signal      `json:"signals"`
	Novelty      *float64      `json:"novelty,omitempty"`
	SimilarFacts []SimilarFact `json:"similar_facts,omitempty"`
}

// Scorer pre-computes embeddings for all canonical phrases at construction
// time and reuses them for every ScoreBlocks call.
type Scorer struct {
	intents    *IntentSet
	intentVecs map[string][][]float32 // intent name -> phrase vectors
	embedder   store.BatchEmbedder
}

// NewScorer pre-embeds the canonical phrases for every intent in the set.
func NewScorer(intents *IntentSet, embedder store.BatchEmbedder) (*Scorer, error) {
	if embedder == nil {
		return nil, fmt.Errorf("NewScorer: embedder is nil")
	}
	if intents == nil || len(intents.Intents) == 0 {
		return nil, fmt.Errorf("NewScorer: intents is empty")
	}
	intentVecs := make(map[string][][]float32, len(intents.Intents))
	for name, intent := range intents.Intents {
		vecs, err := embedder.EmbedBatch(intent.CanonicalPhrases)
		if err != nil {
			return nil, fmt.Errorf("embed phrases for %q: %w", name, err)
		}
		intentVecs[name] = vecs
	}
	return &Scorer{intents: intents, intentVecs: intentVecs, embedder: embedder}, nil
}

// ScoreBlocks returns one BlockResult per input block, with one Signal
// per requested intent name (max cosine similarity vs any phrase).
// Intent names not present in the underlying IntentSet are skipped.
func (s *Scorer) ScoreBlocks(blocks []Block, intentNames []string) []BlockResult {
	results := make([]BlockResult, len(blocks))
	if len(blocks) == 0 {
		return results
	}
	texts := make([]string, len(blocks))
	for i, b := range blocks {
		texts[i] = b.Text
	}
	blockVecs, err := s.embedder.EmbedBatch(texts)
	if err != nil {
		// On embed failure, return empty results rather than crashing
		// the hook. Caller can decide whether to retry or skip the nudge.
		for i := range results {
			results[i] = BlockResult{Index: i}
		}
		return results
	}
	for i := range blocks {
		signals := make([]Signal, 0, len(intentNames))
		for _, name := range intentNames {
			phraseVecs, ok := s.intentVecs[name]
			if !ok {
				continue
			}
			max := 0.0
			for _, pv := range phraseVecs {
				sim := cosine(blockVecs[i], pv)
				if sim > max {
					max = sim
				}
			}
			signals = append(signals, Signal{Intent: name, Score: max})
		}
		results[i] = BlockResult{Index: i, Signals: signals}
	}
	return results
}

// cosine computes cosine similarity between two equal-length vectors.
// Returns 0 if either vector has zero magnitude.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, ma, mb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		ma += fa * fa
		mb += fb * fb
	}
	if ma == 0 || mb == 0 {
		return 0
	}
	return dot / (sqrt(ma) * sqrt(mb))
}

func sqrt(x float64) float64 {
	// Avoid importing math just for this; keep it inline.
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = (z + x/z) / 2
	}
	return z
}
