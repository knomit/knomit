package embeddings

import (
	"os"
	"path/filepath"
	"testing"
)

// ModelCached decides ONE thing: whether knomit-desktop's boot screen says
// "Downloading models…" or "Starting the search engine…". Both mistakes are
// silent — a wrong answer produces a caption that lies, never an error — so the
// cases below are pinned rather than left to inspection.

func testModel() Model {
	return Model{
		ID:           "testmodel",
		ModelURL:     "https://example.invalid/onnx/model_fp16.onnx",
		DataURL:      "https://example.invalid/onnx/model_fp16.onnx_data",
		TokenizerURL: "https://example.invalid/tokenizer.json",
	}
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
}

func TestModelCached_FalseOnAColdHome(t *testing.T) {
	if ModelCached(testModel(), t.TempDir()) {
		t.Error("an empty cache reported the model as present; a first launch would not say it is downloading")
	}
}

func TestModelCached_TrueOnceEveryArtifactIsThere(t *testing.T) {
	cache := t.TempDir()
	writeFiles(t, filepath.Join(cache, "testmodel"),
		"model_fp16.onnx", "model_fp16.onnx_data", "tokenizer.json")

	if !ModelCached(testModel(), cache) {
		t.Error("a fully populated cache reported a download was still needed")
	}
}

// The case that matters most, because it is the one a naive "does the model
// directory exist?" check gets wrong: the graph is a few hundred KB and lands
// in a second, while the external weights are ~617 MB and take minutes. A run
// interrupted between them leaves a directory that LOOKS populated.
func TestModelCached_FalseWhenOnlySomeArtifactsLanded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present []string
	}{
		{"graph only", []string{"model_fp16.onnx"}},
		{"graph and weights, no tokenizer", []string{"model_fp16.onnx", "model_fp16.onnx_data"}},
		{"weights missing", []string{"model_fp16.onnx", "tokenizer.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			writeFiles(t, filepath.Join(cache, "testmodel"), tc.present...)
			if ModelCached(testModel(), cache) {
				t.Errorf("a partial cache (%v) reported complete", tc.present)
			}
		})
	}
}

// A model with no external-weights file must not be judged against a path that
// EnsureModel would never fetch — otherwise it could never report as cached.
func TestModelCached_IgnoresTheDataFileWhenTheModelHasNone(t *testing.T) {
	m := testModel()
	m.DataURL = ""
	cache := t.TempDir()
	writeFiles(t, filepath.Join(cache, "testmodel"), "model_fp16.onnx", "tokenizer.json")

	if !ModelCached(m, cache) {
		t.Error("a model with no external data was judged against one anyway")
	}
}

// modelPaths is shared with EnsureModel precisely so the two cannot disagree
// about where a file lives. This pins the layout both of them depend on.
func TestModelPaths_UsesTheURLBasenamesUnderTheModelID(t *testing.T) {
	modelPath, dataPath, tokPath := modelPaths(testModel(), filepath.Join("root", "models"))

	want := filepath.Join("root", "models", "testmodel")
	if got := filepath.Dir(modelPath); got != want {
		t.Errorf("model dir = %q, want %q", got, want)
	}
	// The basenames are NOT free to change: onnxruntime resolves the external
	// weights by the name embedded in the graph, so renaming breaks loading.
	if got := filepath.Base(modelPath); got != "model_fp16.onnx" {
		t.Errorf("model basename = %q, want the URL's", got)
	}
	if got := filepath.Base(dataPath); got != "model_fp16.onnx_data" {
		t.Errorf("data basename = %q, want the URL's", got)
	}
	if got := filepath.Base(tokPath); got != "tokenizer.json" {
		t.Errorf("tokenizer basename = %q, want the URL's", got)
	}
}
