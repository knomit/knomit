package params

import "testing"

// TestEveryDescriptorReadsItsThresholdsFromParams retires the split rather than
// fixing one instance of it.
//
// Moving embeddinggemma's thresholds into params fixed the model that had an
// inline block; nothing stops the NEXT descriptor from declaring its own, and
// then ForModel — the cgo-free answer — is one model short again, discovered
// only when a caller needs the shipped band. This asserts the property for
// every registered model, so a new descriptor that inlines its numbers fails
// here. (It used to live in internal/embeddings, which owned the registry
// until the registry moved here.)
func TestEveryDescriptorReadsItsThresholdsFromParams(t *testing.T) {
	if len(registry) == 0 {
		t.Fatal("empty registry: the check below would pass vacuously")
	}
	for id, m := range registry {
		want, ok := ForModel(id)
		if !ok {
			t.Errorf("model %q has no entry in modelThresholds; put its calibration there, not in the descriptor", id)
			continue
		}
		if m.Thresholds != want {
			t.Errorf("model %q descriptor thresholds diverge from ForModel(%q):\n  descriptor: %+v\n  params:     %+v",
				id, id, m.Thresholds, want)
		}
	}
}

// The shipped model must actually be registered — DefaultModelID naming a model
// with no descriptor would fail only at first use.
func TestDefaultModelIsRegistered(t *testing.T) {
	if _, err := Lookup(DefaultModelID); err != nil {
		t.Fatalf("Lookup(%q): %v", DefaultModelID, err)
	}
}
