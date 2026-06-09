package embeddings

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/rs/zerolog/log"
)

// EnsureModel ensures model m's ONNX file (+ external data + tokenizer) exist
// under <cacheDir>/<m.ID>/, downloading any that are missing. Files are saved
// under their ORIGINAL URL basenames because ONNX Runtime resolves a model's
// external-weights file by the name embedded in the graph (e.g.
// "model_fp16.onnx_data"); renaming would break that resolution.
// Returns (modelPath, tokenizerPath, error).
func EnsureModel(m Model, cacheDir string) (string, string, error) {
	dir := filepath.Join(cacheDir, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create cache dir: %w", err)
	}
	modelPath := filepath.Join(dir, path.Base(m.ModelURL))
	tokPath := filepath.Join(dir, path.Base(m.TokenizerURL))
	if err := downloadIfMissing(modelPath, m.ModelURL, m.ID+" ONNX model"); err != nil {
		return "", "", err
	}
	if m.DataURL != "" {
		dataPath := filepath.Join(dir, path.Base(m.DataURL))
		if err := downloadIfMissing(dataPath, m.DataURL, m.ID+" ONNX external data"); err != nil {
			return "", "", err
		}
	}
	if err := downloadIfMissing(tokPath, m.TokenizerURL, m.ID+" tokenizer"); err != nil {
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
