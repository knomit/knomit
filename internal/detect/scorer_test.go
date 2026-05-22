package detect

import (
	"errors"
	"math"
	"testing"
)

// fakeEmbedder returns a deterministic vector keyed off the input string.
// It maps every input text to a 2-D vector based on a hash so different
// inputs get different vectors. Used to exercise the cosine math without
// pulling in a real embedder.
type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(text string) ([]float32, error) {
	if text == "" {
		return nil, errors.New("empty input")
	}
	// crude but deterministic 2-D embedding
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
		return []float32{1, 0}, nil
	}
	return []float32{x / mag, y / mag}, nil
}

func (f *fakeEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := f.Embed(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func TestScorer_BlockMatchingCanonical(t *testing.T) {
	intents := &IntentSet{
		Intents: map[string]*Intent{
			"correction": {CanonicalPhrases: []string{"that's wrong"}},
			"discovery":  {CanonicalPhrases: []string{"I see now"}},
		},
		Thresholds: Thresholds{IntentScore: 0.5},
	}
	s, err := NewScorer(intents, &fakeEmbedder{})
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
	// Result for block 0 should have the highest score on "correction"
	top := topSignal(results[0].Signals)
	if top.Intent != "correction" {
		t.Errorf("block 0 top intent = %q, want %q", top.Intent, "correction")
	}
	if top.Score < 0.99 { // identical text → near-1 cosine
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
	intents := &IntentSet{
		Intents: map[string]*Intent{
			"a": {CanonicalPhrases: []string{"alpha"}},
			"b": {CanonicalPhrases: []string{"beta"}},
			"c": {CanonicalPhrases: []string{"gamma"}},
		},
	}
	s, _ := NewScorer(intents, &fakeEmbedder{})
	res := s.ScoreBlocks([]Block{{Text: "hello"}}, []string{"a", "b", "c"})
	if len(res[0].Signals) != 3 {
		t.Errorf("got %d signals, want 3 (one per requested intent)", len(res[0].Signals))
	}
}
