// Package embeddings turns text into vectors via in-process ONNX inference.
// Models are described by a Model descriptor and selected from a built-in
// registry; see model.go for the descriptor and the supported models.
package embeddings

import (
	"fmt"
	"sort"

	"knomit/internal/retrieval"
)

// Pooling selects how a model's token-level output is reduced to one vector.
type Pooling int

const (
	// PoolNone: the ONNX graph already emits a single pooled+normalized vector
	// (e.g. EmbeddingGemma's `sentence_embedding` output). Read it directly.
	PoolNone Pooling = iota
	// PoolMean: mean over sequence length of last_hidden_state (nomic).
	PoolMean
	// PoolCLS: take token 0 of last_hidden_state.
	PoolCLS
	// PoolLastToken: take the last non-pad token (decoder embedding models).
	PoolLastToken
)

// Model is a self-contained description of an embedding model: where to fetch
// it, its ONNX tensor names, how to pool, its dimension, and the retrieval
// prompt templates. Templates carry {content} and optional {title} placeholders.
type Model struct {
	ID           string
	ModelURL     string
	DataURL      string // external-weights file (.onnx_data); "" if none
	TokenizerURL string
	// Optional lowercase-hex SHA-256 pins for the three downloads, verified
	// before the file is renamed into the cache. Empty means "not pinned" —
	// the downloader then falls back to Content-Length verification, which
	// still catches the truncated-response case but not a substituted file.
	//
	// These are unset for the registry entries below: the URLs point at
	// mutable HuggingFace `main` refs, so a pin here would break the download
	// whenever upstream republishes. Pin them (and move the URLs to an
	// immutable revision) if model provenance ever needs to be guaranteed.
	ModelSHA256     string
	DataSHA256      string
	TokenizerSHA256 string
	ONNXInputs      []string
	ONNXOutputs     []string
	Pooling         Pooling
	Dim             int
	// MaxTokens caps the tokenized sequence length fed to the ONNX graph.
	// Sequences longer than this are truncated (with a warning) so a single
	// oversized fact cannot exceed the model's max position embeddings — which
	// would otherwise crash inference or yield an out-of-distribution vector the
	// calibrated thresholds were never measured against. Must be > 0.
	MaxTokens     int
	QueryTemplate string
	DocTemplate   string
	// Thresholds are this model's cosine cutoffs for dedup/search/graph/reflect.
	// They are calibrated per model against the real corpus (tools/calibrate),
	// because each model has a different cosine distribution.
	Thresholds retrieval.Thresholds
}

const hfBase = "https://huggingface.co"

var registry = map[string]Model{
	"embeddinggemma": {
		ID:            "embeddinggemma",
		ModelURL:      hfBase + "/onnx-community/embeddinggemma-300m-ONNX/resolve/main/onnx/model_fp16.onnx",
		DataURL:       hfBase + "/onnx-community/embeddinggemma-300m-ONNX/resolve/main/onnx/model_fp16.onnx_data",
		TokenizerURL:  hfBase + "/onnx-community/embeddinggemma-300m-ONNX/resolve/main/tokenizer.json",
		ONNXInputs:    []string{"input_ids", "attention_mask"},
		ONNXOutputs:   []string{"sentence_embedding"},
		Pooling:       PoolNone,
		Dim:           768,
		MaxTokens:     2048, // EmbeddingGemma max position embeddings.
		QueryTemplate: "task: search result | query: {content}",
		DocTemplate:   "title: {title} | text: {content}",
		// Calibrated against the real knomit corpus (712 facts, tools/calibrate).
		// EmbeddingGemma's cosine distribution runs markedly cooler than nomic's
		// (distinct same-category pairs: mean 0.48 vs 0.75), so every cutoff is
		// ported DOWN by preserving the percentile it occupied on nomic. Dedup
		// 0.82 sits in the validated safety gap (distinct p99 0.77 < 0.82 < true
		// near-dup p05 0.96). SearchFloor's pure port was ~0, clamped to 0.05 to
		// drop only anti-correlated noise.
		Thresholds: retrieval.Thresholds{
			Dedup:          0.82,
			ReflectNovelty: 0.69,
			SimilarTo:      0.18,
			SearchFloor:    0.05,
			RerankHigh:     0.43,
			RerankLow:      0.10,
		},
	},
	"nomic-v1.5": {
		ID:            "nomic-v1.5",
		ModelURL:      hfBase + "/nomic-ai/nomic-embed-text-v1.5/resolve/main/onnx/model_quantized.onnx",
		TokenizerURL:  hfBase + "/nomic-ai/nomic-embed-text-v1.5/resolve/main/tokenizer.json",
		ONNXInputs:    []string{"input_ids", "attention_mask", "token_type_ids"},
		ONNXOutputs:   []string{"last_hidden_state"},
		Pooling:       PoolMean,
		Dim:           768,
		MaxTokens:     2048, // nomic-embed-text-v1.5 context window.
		QueryTemplate: "search_query: {content}",
		DocTemplate:   "search_document: {content}",
		// nomic was the original default; these are the historical literals the
		// thresholds were implicitly tuned against (== retrieval.Defaults()).
		Thresholds: retrieval.Defaults(),
	},
}

// Lookup returns the descriptor for id, or an error listing valid ids.
func Lookup(id string) (Model, error) {
	if m, ok := registry[id]; ok {
		return m, nil
	}
	return Model{}, fmt.Errorf("unknown embedding model %q; valid: %v", id, IDs())
}

// IDs returns the sorted set of registered model ids.
func IDs() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DefaultModelID is the shipped default (validated best on the knomit corpus).
// config.Defaults() mirrors this literal ("embeddinggemma") because the config
// package cannot import this one (it carries cgo); keep the two in sync.
const DefaultModelID = "embeddinggemma"
