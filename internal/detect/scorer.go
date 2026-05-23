package detect

import (
	"fmt"
	"math"

	"github.com/rs/zerolog/log"

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
	results, _ := s.scoreBlocksWithVecs(blocks, intentNames)
	return results
}

// scoreBlocksWithVecs is the shared scoring core. Returns the results AND the
// block embeddings so ScoreBlocksWithNovelty can reuse them without paying
// for a second EmbedBatch call. On embed failure, returns zero-signal results
// (caller decides whether to retry / skip) and a nil vecs slice — the failure
// is logged at Warn so it shows up in the bridge log instead of presenting as
// "everything scored 0".
func (s *Scorer) scoreBlocksWithVecs(blocks []Block, intentNames []string) ([]BlockResult, [][]float32) {
	results := make([]BlockResult, len(blocks))
	if len(blocks) == 0 {
		return results, nil
	}
	texts := make([]string, len(blocks))
	for i, b := range blocks {
		texts[i] = b.Text
	}
	blockVecs, err := s.embedder.EmbedBatch(texts)
	if err != nil {
		log.Warn().Err(err).Int("blocks", len(blocks)).Msg("detect: embedder failed; returning empty signals")
		for i := range results {
			results[i] = BlockResult{Index: i}
		}
		return results, nil
	}
	for i := range blocks {
		signals := make([]Signal, 0, len(intentNames))
		for _, name := range intentNames {
			phraseVecs, ok := s.intentVecs[name]
			if !ok {
				continue
			}
			maxSim := 0.0
			for _, pv := range phraseVecs {
				sim := cosine(blockVecs[i], pv)
				if sim > maxSim {
					maxSim = sim
				}
			}
			signals = append(signals, Signal{Intent: name, Score: maxSim})
		}
		results[i] = BlockResult{Index: i, Signals: signals}
	}
	return results, blockVecs
}

// cosine computes cosine similarity between two equal-length vectors.
// Returns 0 if either vector has zero magnitude. Length mismatches log at
// Warn because that's a developer bug (an intent vector encoded with a
// different embedder than the block vectors), not a runtime condition.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		log.Warn().Int("a", len(a)).Int("b", len(b)).Msg("detect: cosine dim mismatch — returning 0")
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
	return dot / (math.Sqrt(ma) * math.Sqrt(mb))
}

//go:generate go run go.uber.org/mock/mockgen -destination=mock_fact_searcher_test.go -package=detect knomit/internal/detect FactSearcher
//go:generate go run go.uber.org/mock/mockgen -destination=mock_batch_embedder_test.go -package=detect -build_flags=-tags=sqlite_vtable knomit/internal/store BatchEmbedder
//go:generate go run go.uber.org/mock/mockgen -destination=../web/mock_block_scorer_test.go -package=web knomit/internal/detect BlockScorer

// FactSearcher finds existing facts close to a given embedding.
// Implemented in production by the store's search index.
type FactSearcher interface {
	NearestFacts(vec []float32, k int) ([]SimilarFact, error)
}

// BlockScorer is the scoring surface used by HTTP handlers.
type BlockScorer interface {
	ScoreBlocks(blocks []Block, intentNames []string) []BlockResult
	ScoreBlocksWithNovelty(blocks []Block, intentNames []string, searcher FactSearcher) []BlockResult
}

// noveltyK is the number of similar facts to fetch per block when novelty
// scoring is requested. Fixed at 3 — the highest similarity dominates the
// novelty score anyway, and three keeps the search cost bounded.
const noveltyK = 3

// ScoreBlocksWithNovelty is ScoreBlocks plus a per-block novelty score
// and similar-facts list, computed against the provided FactSearcher.
// Reuses the block embeddings produced for intent scoring; no second
// EmbedBatch call.
func (s *Scorer) ScoreBlocksWithNovelty(
	blocks []Block, intentNames []string, searcher FactSearcher,
) []BlockResult {
	results, vecs := s.scoreBlocksWithVecs(blocks, intentNames)
	if searcher == nil || vecs == nil {
		return results
	}
	for i := range results {
		similar, err := searcher.NearestFacts(vecs[i], noveltyK)
		if err != nil {
			log.Warn().Err(err).Int("block", i).Msg("detect: NearestFacts failed; novelty omitted for block")
			continue
		}
		maxSim := 0.0
		for _, sf := range similar {
			if sf.Similarity > maxSim {
				maxSim = sf.Similarity
			}
		}
		novelty := 1 - maxSim
		results[i].Novelty = &novelty
		results[i].SimilarFacts = similar
	}
	return results
}
