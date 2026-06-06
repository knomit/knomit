package embeddings

import "fmt"

// EmbedDocuments embeds (title, body) pairs using the doc template. First cut:
// loops EmbedDocument (correctness first; a true batched ONNX path can be added
// later behind this same signature).
func (e *Embedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	if len(titles) != len(bodies) {
		return nil, fmt.Errorf("EmbedDocuments: %d titles vs %d bodies", len(titles), len(bodies))
	}
	out := make([][]float32, len(bodies))
	for i := range bodies {
		v, err := e.EmbedDocument(titles[i], bodies[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
