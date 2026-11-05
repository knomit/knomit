package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sync"

	"knomit/internal/engine/llm"
	"knomit/internal/engine/store/model"
)

// toolVectors caches one document vector per (embedder, name, description)
// for the life of the process, so a toolset is embedded once and a changed
// description recomputes only that tool. Nothing is stored on disk.
var (
	toolVectorsMu sync.Mutex
	toolVectors   = map[[sha256.Size]byte][]float32{}
)

func toolVectorKey(embedderID string, t llm.ToolDef) [sha256.Size]byte {
	return sha256.Sum256([]byte(embedderID + "\x00" + t.Name + "\x00" + t.Description))
}

type embeddingRanker struct {
	emb   model.Embedder
	tools []llm.ToolDef
}

// NewEmbeddingRanker ranks tools by cosine between the message
// (EmbedQuery) and each tool's document (EmbedDocument(name, description)).
//
// It selects nothing and applies no similarity cutoff: the embedder's recall
// cutoffs were swept on fact bodies, tool descriptions are another population
// (invariant 9f110192), so the caller takes tools by rank under a token budget.
// Any embedder error is returned unchanged; the loop treats it as "no guess"
// and a caller may fall back to the lexical ranker.
func NewEmbeddingRanker(emb model.Embedder, tools []llm.ToolDef) llm.ToolRanker {
	return &embeddingRanker{emb: emb, tools: append([]llm.ToolDef(nil), tools...)}
}

func (r *embeddingRanker) Rank(ctx context.Context, message string) ([]llm.ScoredTool, error) {
	docs := make([][]float32, len(r.tools))
	for i, t := range r.tools {
		v, err := r.document(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("embedding tool %s: %w", t.Name, err)
		}
		docs[i] = v
	}
	q, err := r.emb.EmbedQuery(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("embedding the message: %w", err)
	}
	out := make([]llm.ScoredTool, len(r.tools))
	for i, t := range r.tools {
		out[i] = llm.ScoredTool{Name: t.Name, Score: cosine(q, docs[i])}
	}
	llm.SortScored(out)
	return out, nil
}

func (r *embeddingRanker) document(ctx context.Context, t llm.ToolDef) ([]float32, error) {
	key := toolVectorKey(r.emb.ID(), t)
	toolVectorsMu.Lock()
	v, ok := toolVectors[key]
	toolVectorsMu.Unlock()
	if ok {
		return v, nil
	}
	v, err := r.emb.EmbedDocument(ctx, t.Name, t.Description)
	if err != nil {
		return nil, err
	}
	toolVectorsMu.Lock()
	toolVectors[key] = v
	toolVectorsMu.Unlock()
	return v, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
