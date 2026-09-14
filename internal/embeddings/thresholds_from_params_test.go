package embeddings

import (
	"testing"

	"knomit/internal/embeddings/params"
)

// TestEveryDescriptorReadsItsThresholdsFromParams retires the split rather than
// fixing one instance of it.
//
// Moving embeddinggemma's thresholds into params fixed the model that had an
// inline block; nothing stops the NEXT descriptor from declaring its own, and
// then the cgo-free half of the contract is one model short again — the same
// way it was for gemma, discovered only when a caller needed the shipped band
// without linking ONNX. This asserts the property for every registered model,
// so a new descriptor that inlines its numbers fails here instead of being
// found later by whoever retypes them.
//
// It lives in internal/embeddings because this is where the cgo dependency is
// already paid for. params itself cannot make this assertion: reaching the
// registry would give it an import of its own parent, which is exactly what
// TestParamsHasNoDependencies forbids.
func TestEveryDescriptorReadsItsThresholdsFromParams(t *testing.T) {
	for id, m := range registry {
		want, ok := params.ForModel(id)
		if !ok {
			t.Errorf("model %q has no entry in params.modelThresholds; put its calibration there, not in the descriptor", id)
			continue
		}
		if m.Thresholds != want {
			t.Errorf("model %q descriptor thresholds diverge from params.ForModel(%q):\n  descriptor: %+v\n  params:     %+v",
				id, id, m.Thresholds, want)
		}
	}
}

// The shipped model must actually be registered — DefaultModelID naming a model
// with no descriptor would fail only at first use.
func TestDefaultModelIsRegistered(t *testing.T) {
	if _, err := Lookup(params.DefaultModelID); err != nil {
		t.Fatalf("Lookup(%q): %v", params.DefaultModelID, err)
	}
}
