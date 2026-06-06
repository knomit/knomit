package embeddings

import "testing"

func TestLookupKnownModels(t *testing.T) {
	for _, id := range []string{"embeddinggemma", "nomic-v1.5", "qwen3-0.6b"} {
		m, err := Lookup(id)
		if err != nil {
			t.Fatalf("Lookup(%q) error: %v", id, err)
		}
		if m.ID != id {
			t.Errorf("ID = %q, want %q", m.ID, id)
		}
		if m.Dim <= 0 || m.TokenizerURL == "" || m.ModelURL == "" {
			t.Errorf("%q: incomplete descriptor: %+v", id, m)
		}
		if len(m.ONNXInputs) == 0 || len(m.ONNXOutputs) == 0 {
			t.Errorf("%q: missing ONNX I/O names", id)
		}
	}
}

func TestLookupUnknownListsValidIDs(t *testing.T) {
	_, err := Lookup("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	for _, id := range []string{"embeddinggemma", "nomic-v1.5", "qwen3-0.6b"} {
		if !contains(err.Error(), id) {
			t.Errorf("error %q should list valid id %q", err.Error(), id)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestEmbeddingGemmaDescriptorShape(t *testing.T) {
	m, _ := Lookup("embeddinggemma")
	if m.Pooling != PoolNone {
		t.Errorf("gemma Pooling = %v, want PoolNone (graph pre-pools)", m.Pooling)
	}
	if m.Dim != 768 {
		t.Errorf("gemma Dim = %d, want 768", m.Dim)
	}
	if m.ONNXOutputs[0] != "sentence_embedding" {
		t.Errorf("gemma output = %q, want sentence_embedding", m.ONNXOutputs[0])
	}
}
