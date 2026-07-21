package llm

import (
	"context"
	"encoding/json"
	"knomit/internal/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ollamaTestServer starts an httptest server that answers the health check
// NewOllamaAdapter performs at construction and delegates /api/chat to chat.
// It returns the server and a counter of /api/chat requests only — the health
// check must never be mistaken for a completion attempt when a test asserts on
// how many times the model was actually called.
func ollamaTestServer(t *testing.T, chat http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		calls.Add(1)
		chat(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
	return srv, &calls
}

// writeOllamaChunk writes one NDJSON line in the shape OllamaAdapter.Complete
// parses, flushing so the client observes it as a streamed delta rather than a
// buffered whole-body response.
func writeOllamaChunk(t *testing.T, w http.ResponseWriter, text string, done bool) {
	t.Helper()
	line, err := json.Marshal(ollamaStreamLine{Message: ollamaMessage{Role: "assistant", Content: text}, Done: done})
	require.NoError(t, err)
	_, _ = w.Write(append(line, '\n'))
	w.(http.Flusher).Flush()
}

// TestOllamaComplete_RetriesPreStreamFailure — anchor C1/C3. The server rejects
// the first completion with a 503 (an overload response, the class the policy
// treats as retryable) and streams normally on the second. Nothing reached the
// caller before the failure, so retrying is invisible to them: they see one
// successful completion.
func TestOllamaComplete_RetriesPreStreamFailure(t *testing.T) {
	var attempts atomic.Int32
	_, calls := ollamaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("server overloaded"))
			return
		}
		writeOllamaChunk(t, w, "hello", true)
	})

	adapter, err := NewAdapter(context.Background(), "ollama", "test-model")
	require.NoError(t, err)

	out, err := adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, CompletionOptions{}, nil)
	require.NoError(t, err, "a pre-stream overload response must be retried, not surfaced")
	require.Equal(t, "hello", out)
	require.Equal(t, int32(2), calls.Load(), "exactly one retry")
}

// TestComplete_NoRetryAfterPartialStream — anchor C1's gate, and the reason the
// gemini cache retry checks `accumulated == ""` (gemini.go:177). The server
// streams two chunks and then aborts the connection. The failure is transport
// level and would otherwise be retryable, but chunks already reached the
// caller: replaying the request would emit them a second time, so the error
// must surface as-is.
//
// This passes before the decorator exists (nothing retries yet) and is written
// first deliberately — it is the rail that catches a naive always-retry
// implementation, whose damage is silent duplicate output rather than a crash.
func TestComplete_NoRetryAfterPartialStream(t *testing.T) {
	_, calls := ollamaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOllamaChunk(t, w, "par", false)
		writeOllamaChunk(t, w, "tial", false)
		// Kills the connection mid-stream without logging; the client sees an
		// unexpected EOF while reading the body.
		panic(http.ErrAbortHandler)
	})

	adapter, err := NewAdapter(context.Background(), "ollama", "test-model")
	require.NoError(t, err)

	var chunks []string
	_, err = adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}},
		CompletionOptions{}, func(s string) { chunks = append(chunks, s) })
	require.Error(t, err, "a mid-stream drop is a hard error once output has been emitted")
	require.Equal(t, []string{"par", "tial"}, chunks, "each chunk must reach the caller exactly once")
	require.Equal(t, int32(1), calls.Load(), "no retry may happen after any chunk was emitted")
}

// batchStub is a minimal BatchAdapter used to prove the decorator stack keeps
// the optional interface reachable. It is deliberately not a real provider:
// the property under test is type preservation through the wrappers, which no
// provider behaviour influences.
type batchStub struct{ batchCalls int }

func (b *batchStub) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	return "ok", nil
}
func (b *batchStub) Model() string      { return "batch-stub" }
func (b *batchStub) BatchEnabled() bool { return true }
func (b *batchStub) CompleteBatch(ctx context.Context, requests []BatchRequest, opts CompletionOptions) ([]string, error) {
	b.batchCalls++
	return []string{"batched"}, nil
}

// TestNewAdapter_PreservesBatchAdapter — anchor C4. A decorator that returns a
// plain LLMAdapter erases BatchAdapter from the concrete type, and the loss is
// undetectable at compile time: the type assertion consumers use simply starts
// returning false. Both decorators in the stack must therefore preserve it.
//
// The TracingAdapter half fails against the pre-C4 code — that erasure is a
// real, already-committed bug, not a hypothetical one.
func TestNewAdapter_PreservesBatchAdapter(t *testing.T) {
	t.Run("NewAdapter", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "test-key-not-used-offline")
		adapter, err := NewAdapter(context.Background(), "gemini", "gemini-2.5-flash", config.LLMConfig{Batch: true})
		require.NoError(t, err)

		ba, ok := adapter.(BatchAdapter)
		require.True(t, ok, "the production wrap path must not erase BatchAdapter")
		require.True(t, ba.BatchEnabled())
	})

	t.Run("NewTracingAdapter", func(t *testing.T) {
		inner := &batchStub{}
		tracer, err := NewTracingAdapter(inner, filepath.Join(t.TempDir(), "trace.log"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = tracer.Close() })

		ba, ok := any(tracer).(BatchAdapter)
		require.True(t, ok, "tracing must not erase BatchAdapter")
		require.True(t, ba.BatchEnabled())

		out, err := ba.CompleteBatch(context.Background(), []BatchRequest{{System: "s"}}, CompletionOptions{})
		require.NoError(t, err)
		require.Equal(t, []string{"batched"}, out)
		require.Equal(t, 1, inner.batchCalls, "CompleteBatch must reach the wrapped adapter")
	})
}

// TestOllamaComplete_AttemptTimeout — anchor C2. The server accepts the request
// and never answers. Ollama's http.Client carries no timeout of its own
// (ollama.go:37), so without a per-attempt deadline the call blocks for as long
// as the caller's context allows — here, forever. The watchdog below is the
// test: with no attempt timeout it is the only thing that ends the run.
func TestOllamaComplete_AttemptTimeout(t *testing.T) {
	hang := make(chan struct{})
	_, calls := ollamaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	// Registered after the server so it runs before srv.Close (cleanups are
	// LIFO): Close waits for outstanding handlers, and a handler still parked
	// on hang would deadlock the test binary rather than fail it.
	t.Cleanup(func() { close(hang) })

	inner, err := NewAdapter(context.Background(), "ollama", "test-model")
	require.NoError(t, err)
	// A short policy stands in for the 5m production default: the property under
	// test is that a per-attempt deadline exists at all, not its magnitude.
	adapter := wrapResilient(inner, Policy{AttemptTimeout: 300 * time.Millisecond})

	done := make(chan error, 1)
	go func() {
		_, err := adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, CompletionOptions{}, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded, "a stalled attempt must fail with its own deadline")
	case <-time.After(5 * time.Second):
		t.Fatal("Complete never returned: the stalled attempt has no deadline of its own")
	}
	require.Equal(t, int32(1), calls.Load(), "MaxRetries 0 means the timeout is terminal")
}
