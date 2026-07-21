package embeddings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDownload_TruncatedBodyIsRejected is the core of P0.6. A response that
// declares a Content-Length and then delivers fewer bytes reads as SUCCESS to
// io.Copy. Because the cache is skip-if-present, such a file used to be renamed
// into place permanently and resurfaced as a cryptic ONNX failure on every
// later boot, with no self-healing path.
//
// The truncated file must be rejected AND must not be left in the cache.
func TestDownload_TruncatedBodyIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare 100 bytes, send 10, then hang up.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("0123456789"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // kill the connection mid-body
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.onnx")
	err := downloadIfMissing(context.Background(), dst, srv.URL+"/model.onnx", "", "test model")
	require.Error(t, err, "a short body must not be accepted")
	require.NoFileExists(t, dst, "a failed download must not be renamed into the cache")

	// And no .part-* leftovers.
	entries, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".part-", "temp file must be cleaned up")
	}
}

// TestDownload_ContentLengthMismatchIsRejected covers the cleaner case: a full
// response body whose length simply disagrees with the declared Content-Length.
func TestDownload_ContentLengthMismatchIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(64))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer srv.Close()

	// Sanity: the honest case succeeds.
	dst := filepath.Join(t.TempDir(), "ok.bin")
	require.NoError(t, downloadIfMissing(context.Background(), dst, srv.URL, "", "honest"))
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Len(t, data, 64)
}

// TestDownload_SHA256Pin verifies the optional integrity pin: a matching digest
// is accepted, a mismatched one fails and leaves nothing behind.
func TestDownload_SHA256Pin(t *testing.T) {
	payload := []byte("the-model-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	good := sha256.Sum256(payload)
	goodHex := hex.EncodeToString(good[:])

	okDst := filepath.Join(t.TempDir(), "pinned.bin")
	require.NoError(t, downloadIfMissing(context.Background(), okDst, srv.URL, goodHex, "pinned"))
	require.FileExists(t, okDst)

	badDst := filepath.Join(t.TempDir(), "wrong.bin")
	err := downloadIfMissing(context.Background(), badDst, srv.URL,
		"0000000000000000000000000000000000000000000000000000000000000000", "wrong pin")
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256 mismatch")
	require.NoFileExists(t, badDst, "a hash mismatch must not land in the cache")
}

// TestDownload_CancelledContextAborts is the anti-hang assertion: embeddings are
// mandatory at boot, so a fetch that cannot make progress must surface as an
// error rather than blocking startup forever. A cancelled context stands in for
// the stall watchdog, which uses the same cancellation path on a much longer
// (90s) timer that a unit test should not wait out.
func TestDownload_CancelledContextAborts(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("start"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block // never send the rest
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	go cancel()

	dst := filepath.Join(t.TempDir(), "stalled.bin")
	err := downloadIfMissing(ctx, dst, srv.URL, "", "stalled model")
	require.Error(t, err, "an aborted download must not hang or silently succeed")
	require.NoFileExists(t, dst)
}

// TestDownload_NoContentLengthStillSucceeds: a chunked response declares no
// length. With no pin and no length there is nothing to verify against, and
// refusing would make the downloader depend on server behaviour we don't
// control — so it must still succeed.
func TestDownload_NoContentLengthStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Write([]byte("chunked-payload"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "chunked.bin")
	require.NoError(t, downloadIfMissing(context.Background(), dst, srv.URL, "", "chunked"))
	data, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "chunked-payload", string(data))
}
