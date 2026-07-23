package embeddings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	// dialTimeout / tlsTimeout / headerTimeout bound the phases that should be
	// fast regardless of file size. They must NOT be a whole-request timeout:
	// the model is hundreds of MB and a legitimate download over a slow link can
	// take many minutes.
	dialTimeout   = 30 * time.Second
	tlsTimeout    = 30 * time.Second
	headerTimeout = 60 * time.Second

	// stallTimeout aborts a transfer that has stopped making progress. This is
	// what a whole-request timeout is usually reaching for: a connection that
	// accepts but never delivers used to hang server boot FOREVER, because
	// embeddings are mandatory (app.New) and this runs before the server listens.
	stallTimeout = 90 * time.Second

	// stallCheckInterval is how often the watchdog samples the progress counter.
	stallCheckInterval = 5 * time.Second
)

// downloadClient bounds connection setup and response headers but deliberately
// sets no Client.Timeout — see stallTimeout.
var downloadClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsTimeout,
		ResponseHeaderTimeout: headerTimeout,
		Proxy:                 http.ProxyFromEnvironment,
	},
}

// EnsureModel ensures model m's ONNX file (+ external data + tokenizer) exist
// under <cacheDir>/<m.ID>/, downloading any that are missing. Files are saved
// under their ORIGINAL URL basenames because ONNX Runtime resolves a model's
// external-weights file by the name embedded in the graph (e.g.
// "model_fp16.onnx_data"); renaming would break that resolution.
// Returns (modelPath, tokenizerPath, error).
func EnsureModel(ctx context.Context, m Model, cacheDir string) (string, string, error) {
	dir := filepath.Join(cacheDir, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create cache dir: %w", err)
	}
	modelPath := filepath.Join(dir, path.Base(m.ModelURL))
	tokPath := filepath.Join(dir, path.Base(m.TokenizerURL))
	if err := downloadIfMissing(ctx, modelPath, m.ModelURL, m.ModelSHA256, m.ID+" ONNX model"); err != nil {
		return "", "", err
	}
	if m.DataURL != "" {
		dataPath := filepath.Join(dir, path.Base(m.DataURL))
		if err := downloadIfMissing(ctx, dataPath, m.DataURL, m.DataSHA256, m.ID+" ONNX external data"); err != nil {
			return "", "", err
		}
	}
	if err := downloadIfMissing(ctx, tokPath, m.TokenizerURL, m.TokenizerSHA256, m.ID+" tokenizer"); err != nil {
		return "", "", err
	}
	return modelPath, tokPath, nil
}

// downloadIfMissing fetches url to dst unless dst already exists.
//
// The download is verified BEFORE the atomic rename into place, because the
// cache is skip-if-present: a file that lands corrupt is never re-fetched and
// surfaces as a cryptic ONNX failure on every subsequent boot, with no way to
// self-heal short of manually deleting the cache directory.
//
// Two checks, in order of strength:
//   - wantSHA256, when the registry pins one for this file (exact).
//   - Content-Length, whenever the server declares one. This is the check that
//     catches the real failure mode here — a 200 response whose body is cut
//     short mid-transfer, which io.Copy reports as success.
//
// A response with NEITHER a pinned hash nor a Content-Length is accepted; there
// is nothing left to compare against, and refusing would make the downloader
// depend on server behaviour we don't control.
func downloadIfMissing(ctx context.Context, dst, url, wantSHA256, name string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // already present
	}
	log.Info().Str("url", url).Str("dest", dst).Msgf("downloading %s", name)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}

	// Write to a temp file in the SAME directory so the rename below is atomic
	// (a cross-filesystem rename is not).
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".part-*")
	if err != nil {
		return fmt.Errorf("create tmp file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed away
	}()

	var sum hash.Hash
	var w io.Writer = tmp
	if wantSHA256 != "" {
		sum = sha256.New()
		w = io.MultiWriter(tmp, sum)
	}

	body := &progressReader{r: resp.Body}
	stop := body.watchStalls(ctx, cancel, name)
	written, copyErr := io.Copy(w, body)
	stop()
	if copyErr != nil {
		if body.stalled.Load() {
			return fmt.Errorf("download %s: no data received for %s (stalled at %d bytes)", name, stallTimeout, written)
		}
		return fmt.Errorf("download %s: %w", name, copyErr)
	}

	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return fmt.Errorf("download %s: truncated — got %d bytes, server declared %d", name, written, resp.ContentLength)
	}
	if sum != nil {
		if got := hex.EncodeToString(sum.Sum(nil)); got != wantSHA256 {
			return fmt.Errorf("download %s: sha256 mismatch — got %s, want %s", name, got, wantSHA256)
		}
	}

	// Flush to disk before the rename: without this a crash between rename and
	// writeback can leave a correctly-named file with unwritten tail blocks —
	// exactly the permanently-poisoned cache entry we are guarding against.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename tmp: %w", err)
	}
	log.Info().Str("dest", dst).Int64("bytes", written).Msgf("%s downloaded", name)
	return nil
}

// progressReader counts bytes read so a watchdog can tell "slow" from "dead".
type progressReader struct {
	r       io.Reader
	n       atomic.Int64
	stalled atomic.Bool
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n.Add(int64(n))
	}
	return n, err
}

// watchStalls cancels the request context if no bytes arrive for stallTimeout.
// Returns a function that stops the watchdog. Cancelling makes the in-flight
// Read fail, which surfaces through io.Copy — the stalled flag lets the caller
// report that as a stall rather than a generic context error.
func (p *progressReader) watchStalls(ctx context.Context, cancel context.CancelFunc, name string) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(stallCheckInterval)
		defer ticker.Stop()
		last := p.n.Load()
		idle := time.Duration(0)
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := p.n.Load()
				if now != last {
					last, idle = now, 0
					continue
				}
				idle += stallCheckInterval
				if idle >= stallTimeout {
					log.Warn().Str("name", name).Int64("bytes", now).
						Msgf("download stalled — no data for %s, aborting", stallTimeout)
					p.stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}
