package llm

import (
	"context"
	"encoding/json"
	"errors"
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
// gemini cache retry checks `accumulated == ""` (gemini.go:177). Once a chunk
// has reached the caller, replaying the request would emit it a second time, so
// the error must surface as-is however retryable it looks.
//
// The mid-stream failure has to be one isRetryable() actually accepts, or the
// test proves nothing: an aborted connection surfaces as io.ErrUnexpectedEOF,
// which is already non-retryable, so the retry path is never reached and the
// gate is never consulted. (An earlier version of this test made exactly that
// mistake and passed with `emitted ||` deleted from the gate.) The server here
// therefore streams two chunks and then hangs, letting the attempt timeout fire
// — context.DeadlineExceeded, which isRetryable returns true for, on a policy
// with retries left to spend. The only thing standing between this and three
// upstream calls is the gate.
func TestComplete_NoRetryAfterPartialStream(t *testing.T) {
	hang := make(chan struct{})
	_, calls := ollamaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOllamaChunk(t, w, "par", false)
		writeOllamaChunk(t, w, "tial", false)
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	// Before srv.Close (cleanups are LIFO): Close waits for outstanding
	// handlers, and one still parked on hang would deadlock the test binary.
	t.Cleanup(func() { close(hang) })

	inner, err := NewAdapter(context.Background(), "ollama", "test-model")
	require.NoError(t, err)
	// Retries deliberately available: a policy with MaxRetries 0 could not tell
	// the gate apart from the budget running out.
	adapter := wrapResilient(inner, Policy{AttemptTimeout: 300 * time.Millisecond, MaxRetries: 2})

	var chunks []string
	_, err = adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}},
		CompletionOptions{}, func(s string) { chunks = append(chunks, s) })
	require.ErrorIs(t, err, context.DeadlineExceeded, "the retryable failure must surface once output has been emitted")
	require.Equal(t, []string{"par", "tial"}, chunks, "each chunk must reach the caller exactly once")
	require.Equal(t, int32(1), calls.Load(), "no retry may happen after any chunk was emitted")
}

// TestComplete_RetriesRetryableFailureWithNilOnChunk is the control for the
// test above: same server, same policy, but a nil onChunk, so the gate never
// closes and the identical failure *is* retried. Without this pair, a gate that
// blocked every retry unconditionally would look correct.
func TestComplete_RetriesRetryableFailureWithNilOnChunk(t *testing.T) {
	hang := make(chan struct{})
	_, calls := ollamaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOllamaChunk(t, w, "par", false)
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(hang) })

	inner, err := NewAdapter(context.Background(), "ollama", "test-model")
	require.NoError(t, err)
	adapter := wrapResilient(inner, Policy{AttemptTimeout: 200 * time.Millisecond, MaxRetries: 2})

	_, err = adapter.Complete(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}}, CompletionOptions{}, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, int32(3), calls.Load(), "a caller observing nothing can be retried the full budget")
}

// TestIsOverloadedErr — anchor for the classifier's narrowness. Every string in
// the reject list was observed to retry under a bare substring match on "429" /
// "503" / "529" / "rate limit"; each one costs the full retry budget to fail
// identically, since none of them describes a transient condition.
func TestIsOverloadedErr(t *testing.T) {
	retryable := []string{
		// Real overload responses, in the shapes our adapters produce them.
		`ollama: HTTP 429: {"error":"too many requests"}`,
		`ollama: HTTP 503: server overloaded`,
		`ollama: HTTP 529: overloaded_error`,
		// genai renders API errors as "Error %d, Message: …" (api_client.go:508).
		`Gemini stream error: Error 429, Message: Resource has been exhausted, Status: RESOURCE_EXHAUSTED`,
		`Gemini stream error: Error 503, Message: The model is overloaded, Status: UNAVAILABLE`,
		// JSON error envelopes, and the bare phrases providers use.
		`anthropic: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		`bedrock: {"code": 429, "message": "rate limit exceeded"}`,
		`claudecli: exit status 1: 503 Service Unavailable`,
	}
	for _, msg := range retryable {
		require.True(t, isOverloadedErr(errors.New(msg)), "must retry: %s", msg)
	}

	nonRetryable := []string{
		// The most common ollama 400 by a wide margin — and "14293" contains 429.
		`ollama: HTTP 400: {"error":"input length 14293 exceeds context window 8192"}`,
		`ollama: HTTP 400: {"error":"model 'llama3-503b' not found"}`,
		`ollama: HTTP 404: {"error":"model 'qwen2.5-529b' not found"}`,
		// Request ids are hex-ish and routinely contain these digits.
		`anthropic: invalid_request_error (request_id req_011A429fRkQ)`,
		`bedrock: ValidationException (request id 8b503f2c-0a11-4d0e-9f31-6a2c4b1d7e90)`,
		// Our error strings quote prompts back, and prompts discuss anything.
		`ollama: HTTP 400: {"error":"prompt mentions a rate limit policy of 100 rps"}`,
		// Genuinely permanent failures.
		`anthropic: authentication_error: invalid x-api-key`,
		`gemini: Error 400, Message: API key not valid, Status: INVALID_ARGUMENT`,
	}
	for _, msg := range nonRetryable {
		require.False(t, isOverloadedErr(errors.New(msg)), "must not retry: %s", msg)
	}
}

// stubAdapter is an LLMAdapter whose every call is scripted by the test. It exists
// because the cancellation paths need the parent context cancelled at a precise
// moment relative to an attempt, which no real transport can be asked for.
type stubAdapter struct {
	calls atomic.Int32
	fn    func(call int32) (string, error)
}

func (s *stubAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	return s.fn(s.calls.Add(1))
}
func (s *stubAdapter) Model() string { return "stub" }

// TestComplete_CancelledParentSurfacesCancellation — a parent cancellation must
// reach the caller as one, even when the attempt failed for an unrelated
// reason. Returning only the provider's error erases the cancellation:
// errors.Is(err, context.Canceled) goes false, and a graceful shutdown reads as
// a provider outage to every caller branching on it.
func TestComplete_CancelledParentSurfacesCancellation(t *testing.T) {
	t.Run("cancelled during the attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stub := &stubAdapter{fn: func(int32) (string, error) {
			// Cancelled while in flight; the attempt then fails on its own terms.
			cancel()
			return "", errors.New("ollama: HTTP 503: server overloaded")
		}}
		adapter := wrapResilient(stub, Policy{MaxRetries: 2})

		_, err := adapter.Complete(ctx, "sys", nil, CompletionOptions{}, nil)
		require.ErrorIs(t, err, context.Canceled, "the caller's cancellation must survive to the caller")
		require.Contains(t, err.Error(), "503", "the provider's failure must stay legible for logs")
		require.Equal(t, int32(1), stub.calls.Load(), "a cancelled request must not spend a retry")
	})

	t.Run("cancelled during the backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stub := &stubAdapter{fn: func(int32) (string, error) {
			// Lands inside the backoff, which is at least retryBackoffBase/2.
			time.AfterFunc(20*time.Millisecond, cancel)
			return "", errors.New("ollama: HTTP 503: server overloaded")
		}}
		adapter := wrapResilient(stub, Policy{MaxRetries: 2})

		_, err := adapter.Complete(ctx, "sys", nil, CompletionOptions{}, nil)
		require.ErrorIs(t, err, context.Canceled, "cancellation during backoff must surface as cancellation")
		require.Contains(t, err.Error(), "503")
		require.Equal(t, int32(1), stub.calls.Load(), "the pending retry must be abandoned, not run")
	})
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
