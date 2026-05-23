package detect

import (
	"errors"
	"math"
	"testing"

	"go.uber.org/mock/gomock"
)

// hashEmbed maps a string to a deterministic 2-D unit vector. Identical
// inputs produce identical vectors so cosine similarity tests can lock in
// near-1 scores. Lives in the test file because the production embedder is
// 768-D; this is the smallest implementation that exercises the scoring math
// end-to-end.
func hashEmbed(text string) []float32 {
	if text == "" {
		return []float32{1, 0}
	}
	var x, y float32
	for i, b := range []byte(text) {
		if i%2 == 0 {
			x += float32(b)
		} else {
			y += float32(b)
		}
	}
	mag := float32(math.Sqrt(float64(x*x + y*y)))
	if mag == 0 {
		return []float32{1, 0}
	}
	return []float32{x / mag, y / mag}
}

// embedBatchHashed implements EmbedBatch via hashEmbed. Useful as the
// DoAndReturn body for mockgen-generated MockBatchEmbedder when a test
// wants deterministic-by-content embeddings.
func embedBatchHashed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashEmbed(t)
	}
	return out, nil
}

func TestScorer_BlockMatchingCanonical(t *testing.T) {
	ctrl := gomock.NewController(t)
	embedder := NewMockBatchEmbedder(ctrl)
	// NewScorer embeds the canonical phrases for every intent, then ScoreBlocks
	// embeds the blocks. Both calls return deterministic hashed vectors.
	embedder.EXPECT().EmbedBatch(gomock.Any()).DoAndReturn(embedBatchHashed).AnyTimes()

	intents := &IntentSet{
		Intents: map[string]*Intent{
			"correction": {CanonicalPhrases: []string{"that's wrong"}},
			"discovery":  {CanonicalPhrases: []string{"I see now"}},
		},
		Thresholds: Thresholds{IntentScore: 0.5},
	}
	s, err := NewScorer(intents, embedder)
	if err != nil {
		t.Fatalf("NewScorer: %v", err)
	}
	blocks := []Block{
		{Role: "user", Text: "that's wrong"},
		{Role: "assistant", Text: "totally unrelated lorem ipsum"},
	}
	results := s.ScoreBlocks(blocks, []string{"correction", "discovery"})
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	top := topSignal(results[0].Signals)
	if top.Intent != "correction" {
		t.Errorf("block 0 top intent = %q, want %q", top.Intent, "correction")
	}
	if top.Score < 0.99 {
		t.Errorf("block 0 top score = %v, want >= 0.99", top.Score)
	}
}

func topSignal(sigs []Signal) Signal {
	var top Signal
	for _, s := range sigs {
		if s.Score > top.Score {
			top = s
		}
	}
	return top
}

func TestScorer_AllRequestedIntentsAppearInResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	embedder := NewMockBatchEmbedder(ctrl)
	embedder.EXPECT().EmbedBatch(gomock.Any()).DoAndReturn(embedBatchHashed).AnyTimes()

	intents := &IntentSet{
		Intents: map[string]*Intent{
			"a": {CanonicalPhrases: []string{"alpha"}},
			"b": {CanonicalPhrases: []string{"beta"}},
			"c": {CanonicalPhrases: []string{"gamma"}},
		},
	}
	s, _ := NewScorer(intents, embedder)
	res := s.ScoreBlocks([]Block{{Text: "hello"}}, []string{"a", "b", "c"})
	if len(res[0].Signals) != 3 {
		t.Errorf("got %d signals, want 3 (one per requested intent)", len(res[0].Signals))
	}
}

func TestScorer_NoveltyScoring_ReusesBlockEmbeddingNoSecondCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	embedder := NewMockBatchEmbedder(ctrl)

	// One call for canonical phrases (during NewScorer) plus exactly one call
	// for the blocks (during ScoreBlocksWithNovelty). If the implementation
	// regresses and embeds blocks twice, this fails with "unexpected call".
	gomock.InOrder(
		embedder.EXPECT().EmbedBatch([]string{"that's wrong"}).DoAndReturn(embedBatchHashed),
		embedder.EXPECT().EmbedBatch([]string{"that's wrong"}).DoAndReturn(embedBatchHashed),
	)

	intents := &IntentSet{
		Intents: map[string]*Intent{
			"correction": {CanonicalPhrases: []string{"that's wrong"}},
		},
	}
	s, _ := NewScorer(intents, embedder)

	searcher := NewMockFactSearcher(ctrl)
	searcher.EXPECT().NearestFacts(gomock.Any(), noveltyK).Return(
		[]SimilarFact{{Path: "invariants/architecture/historical-graph", Similarity: 0.42}},
		nil,
	)

	results := s.ScoreBlocksWithNovelty(
		[]Block{{Text: "that's wrong"}},
		[]string{"correction"},
		searcher,
	)
	if results[0].Novelty == nil {
		t.Fatal("Novelty is nil; expected a value")
	}
	if got, want := *results[0].Novelty, 1-0.42; abs(got-want) > 1e-6 {
		t.Errorf("Novelty = %v, want %v", got, want)
	}
	if len(results[0].SimilarFacts) != 1 || results[0].SimilarFacts[0].Path != "invariants/architecture/historical-graph" {
		t.Errorf("SimilarFacts = %+v, want one entry for the historical-graph path", results[0].SimilarFacts)
	}
}

func TestScorer_EmptyBlocks_ReturnsEmpty(t *testing.T) {
	ctrl := gomock.NewController(t)
	embedder := NewMockBatchEmbedder(ctrl)
	// NewScorer still calls EmbedBatch for canonical phrases. ScoreBlocks
	// must not call EmbedBatch when blocks is empty (early-return path).
	embedder.EXPECT().EmbedBatch(gomock.Any()).DoAndReturn(embedBatchHashed).Times(1)

	intents := &IntentSet{
		Intents: map[string]*Intent{"a": {CanonicalPhrases: []string{"x"}}},
	}
	s, _ := NewScorer(intents, embedder)
	res := s.ScoreBlocks(nil, []string{"a"})
	if len(res) != 0 {
		t.Errorf("ScoreBlocks(nil) = %d results, want 0", len(res))
	}
}

func TestScorer_EmbedderFailure_ReturnsZeroSignalsAndNoNovelty(t *testing.T) {
	ctrl := gomock.NewController(t)
	embedder := NewMockBatchEmbedder(ctrl)
	// First EmbedBatch (canonical phrases) succeeds; second (blocks) fails.
	gomock.InOrder(
		embedder.EXPECT().EmbedBatch(gomock.Any()).DoAndReturn(embedBatchHashed),
		embedder.EXPECT().EmbedBatch(gomock.Any()).Return(nil, errors.New("embedder down")),
	)

	intents := &IntentSet{Intents: map[string]*Intent{"a": {CanonicalPhrases: []string{"x"}}}}
	s, _ := NewScorer(intents, embedder)

	// searcher must NOT be queried when block embedding failed.
	searcher := NewMockFactSearcher(ctrl)
	results := s.ScoreBlocksWithNovelty(
		[]Block{{Text: "anything"}},
		[]string{"a"},
		searcher,
	)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if len(results[0].Signals) != 0 {
		t.Errorf("expected zero signals on embed failure, got %+v", results[0].Signals)
	}
	if results[0].Novelty != nil {
		t.Errorf("Novelty should be nil on embed failure, got %v", *results[0].Novelty)
	}
}

func TestCosine_DimensionMismatch_ReturnsZero(t *testing.T) {
	if got := cosine([]float32{1, 0, 0}, []float32{1, 0}); got != 0 {
		t.Errorf("cosine dim mismatch = %v, want 0", got)
	}
}

func TestCosine_ZeroMagnitude_ReturnsZero(t *testing.T) {
	if got := cosine([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Errorf("cosine(zero, _) = %v, want 0", got)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
