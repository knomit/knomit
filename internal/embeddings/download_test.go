package embeddings

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureModelDownloadsPerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data-for-" + filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()

	m := Model{
		ID:           "testmodel",
		ModelURL:     srv.URL + "/model.onnx",
		TokenizerURL: srv.URL + "/tokenizer.json",
	}
	cache := t.TempDir()
	modelPath, tokPath, err := EnsureModel(m, cache)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(modelPath) != filepath.Join(cache, "testmodel") {
		t.Errorf("model not under per-id dir: %s", modelPath)
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Errorf("model not written: %v", err)
	}
	if _, err := os.Stat(tokPath); err != nil {
		t.Errorf("tokenizer not written: %v", err)
	}
	if _, _, err := EnsureModel(m, cache); err != nil { // idempotent
		t.Errorf("second EnsureModel: %v", err)
	}
}

func TestEnsureModelFetchesExternalData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	m := Model{ID: "withdata", ModelURL: srv.URL + "/model.onnx",
		DataURL: srv.URL + "/model.onnx_data", TokenizerURL: srv.URL + "/tokenizer.json"}
	cache := t.TempDir()
	modelPath, _, err := EnsureModel(m, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(modelPath + "_data"); err != nil {
		t.Errorf("external data not written next to model: %v", err)
	}
}
