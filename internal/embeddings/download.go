package embeddings

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
)

const (
	modelURL     = "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/main/onnx/model_quantized.onnx"
	tokenizerURL = "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/main/tokenizer.json"
)

// EnsureModel ensures the nomic-embed-text-v1.5 ONNX model and tokenizer are
// present in cacheDir, downloading them from HuggingFace if missing.
// Returns (modelPath, tokenizerPath, error).
func EnsureModel(cacheDir string) (string, string, error) {
	modelPath := filepath.Join(cacheDir, "model.onnx")
	tokPath := filepath.Join(cacheDir, "tokenizer.json")

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create cache dir: %w", err)
	}

	if err := downloadIfMissing(modelPath, modelURL, "nomic-embed-text-v1.5 ONNX model"); err != nil {
		return "", "", err
	}
	if err := downloadIfMissing(tokPath, tokenizerURL, "tokenizer"); err != nil {
		return "", "", err
	}
	return modelPath, tokPath, nil
}

func downloadIfMissing(dst, url, name string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // already present
	}
	log.Info().Str("url", url).Str("dest", dst).Msgf("downloading %s", name)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", name, err)
	}
	f.Close()
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	log.Info().Str("dest", dst).Msgf("%s downloaded", name)
	return nil
}
