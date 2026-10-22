package modelparams

// The model registry: every embedding model knomit can run, described by data
// alone (where to fetch it, its tensor names, pooling, templates and its
// calibrated thresholds), plus where its artifacts live in the cache.
//
// It moved here from internal/engine/embeddings so a caller that only needs to KNOW
// about a model — knomit-desktop's "Downloading models…" caption asks
// ModelCached — does not link the ONNX runtime and the tokenizers library to
// do it. internal/engine/embeddings re-exports Model, Pooling and Lookup as aliases,
// so the inference code reads exactly as before. Like the rest of this
// package, it imports the standard library only.

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	// ShortStringTemplate renders a BARE SHORT STRING (a fact title, a motif
	// name, a name+definition) for embedding. It is deliberately the title slot
	// with a literal "none" body — the "title-hack".
	//
	// This looks wrong and is right: the task sweep recorded in
	// .claude/plans/motif/2026-08-20-motif-summarizer-experiment.md measured the
	// model card's own task prompts WORSE than this rendering for strings of a
	// few words on embeddinggemma, and every operating point calibrated in that
	// work assumes this exact string. Do not "fix" it to a task prompt without
	// re-running the sweep. New embedding axes get their own calibrated
	// operating points; never reuse fact-embedding thresholds
	// (decisions/embeddings/model-dependent-thresholds-on-descriptor).
	ShortStringTemplate string
	// Thresholds are this model's cosine cutoffs for dedup/search/graph/reflect.
	// They are calibrated per model against the real corpus (tools/calibrate),
	// because each model has a different cosine distribution.
	Thresholds Thresholds
}

const hfBase = "https://huggingface.co"

// mustThresholds reads a model's calibrated set out of params at init time.
//
// It panics rather than defaulting because a descriptor naming a model params
// has no calibration for is a build-time mistake in this file, not a runtime
// condition — and the one thing worse than failing here would be silently
// handing back some other model's geometry. That is the same reason
// ForModel returns a bool instead of falling back to Defaults().
// TestEveryDescriptorReadsItsThresholdsFromParams catches it before a build
// ever runs.
func mustThresholds(id string) Thresholds {
	th, ok := ForModel(id)
	if !ok {
		panic("embeddings: no calibrated thresholds in params for model " + id)
	}
	return th
}

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
		// The title-hack — see the descriptor comment above.
		ShortStringTemplate: "title: {content} | text: none",
		// Calibration (and the comment describing how it was derived) lives in
		// params, so a cgo-free caller can read this model's geometry without
		// linking ONNX. Referenced the same way the nomic descriptor below
		// references Defaults() — one source per model, no inline block.
		Thresholds: mustThresholds(DefaultModelID),
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
		// Not swept on nomic: its document rendering has no title slot, so the
		// title-hack has no analogue here. Re-measure before trusting any
		// short-string operating point under this model.
		ShortStringTemplate: "search_document: {content}",
		// nomic was the original default; these are the historical literals the
		// thresholds were implicitly tuned against (== Defaults()).
		Thresholds: Defaults(),
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

// ModelPaths returns the on-disk locations of m's artifacts under cacheDir.
// dataPath is "" for a model with no external-weights file.
//
// It exists so embeddings.EnsureModel and ModelCached cannot disagree about
// where a file lives: "is it cached?" answered from a second copy of this
// arithmetic is a question that starts returning the wrong answer the moment
// either side is edited.
func ModelPaths(m Model, cacheDir string) (modelPath, dataPath, tokPath string) {
	dir := filepath.Join(cacheDir, m.ID)
	modelPath = filepath.Join(dir, path.Base(m.ModelURL))
	tokPath = filepath.Join(dir, path.Base(m.TokenizerURL))
	if m.DataURL != "" {
		dataPath = filepath.Join(dir, path.Base(m.DataURL))
	}
	return modelPath, dataPath, tokPath
}

// ModelCached reports whether every artifact embeddings.EnsureModel would
// fetch for m is already on disk, i.e. whether starting the server will
// download anything.
//
// It is a LABELLING aid, not a gate: knomit-desktop asks it before it spawns
// the server so the boot screen can say "Downloading models…" only when that
// is true, instead of claiming a download on every warm start. EnsureModel
// remains the authority and re-checks each file itself.
//
// Presence only, deliberately — it does not verify size or hash. A partially
// written file cannot be observed here anyway: downloads land through an
// atomic rename from a .part-* temp file, so a path either does not exist or
// holds a fully verified artifact (embeddings downloadIfMissing).
func ModelCached(m Model, cacheDir string) bool {
	modelPath, dataPath, tokPath := ModelPaths(m, cacheDir)
	for _, p := range []string{modelPath, dataPath, tokPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}
