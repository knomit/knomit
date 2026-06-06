// Package embeddings turns text into vectors via in-process ONNX inference.
// Models are described by a Model descriptor and selected from a built-in
// registry; see model.go for the descriptor and the supported models.
package embeddings

import (
	"fmt"
	"sort"
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
	// PoolLastToken: take the last non-pad token (decoder models, e.g. Qwen3).
	PoolLastToken
)

// Model is a self-contained description of an embedding model: where to fetch
// it, its ONNX tensor names, how to pool, its dimension, and the retrieval
// prompt templates. Templates carry {content} and optional {title} placeholders.
type Model struct {
	ID            string
	ModelURL      string
	DataURL       string // external-weights file (.onnx_data); "" if none
	TokenizerURL  string
	ONNXInputs    []string
	ONNXOutputs   []string
	Pooling       Pooling
	Dim           int
	QueryTemplate string
	DocTemplate   string
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
		QueryTemplate: "task: search result | query: {content}",
		DocTemplate:   "title: {title} | text: {content}",
	},
	"nomic-v1.5": {
		ID:            "nomic-v1.5",
		ModelURL:      hfBase + "/nomic-ai/nomic-embed-text-v1.5/resolve/main/onnx/model_quantized.onnx",
		TokenizerURL:  hfBase + "/nomic-ai/nomic-embed-text-v1.5/resolve/main/tokenizer.json",
		ONNXInputs:    []string{"input_ids", "attention_mask", "token_type_ids"},
		ONNXOutputs:   []string{"last_hidden_state"},
		Pooling:       PoolMean,
		Dim:           768,
		QueryTemplate: "search_query: {content}",
		DocTemplate:   "search_document: {content}",
	},
	"qwen3-0.6b": {
		ID:            "qwen3-0.6b",
		ModelURL:      hfBase + "/onnx-community/Qwen3-Embedding-0.6B-ONNX/resolve/main/onnx/model.onnx",
		DataURL:       hfBase + "/onnx-community/Qwen3-Embedding-0.6B-ONNX/resolve/main/onnx/model.onnx_data",
		TokenizerURL:  hfBase + "/onnx-community/Qwen3-Embedding-0.6B-ONNX/resolve/main/tokenizer.json",
		ONNXInputs:    []string{"input_ids", "attention_mask"},
		ONNXOutputs:   []string{"last_hidden_state"},
		Pooling:       PoolLastToken,
		Dim:           1024,
		QueryTemplate: "Instruct: Given a query, retrieve knowledge-base facts that answer it\nQuery:{content}",
		DocTemplate:   "{content}",
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
const DefaultModelID = "embeddinggemma"
