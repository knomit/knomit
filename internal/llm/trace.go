package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// TracingAdapter wraps an LLMAdapter and writes full request/response
// payloads to a log file for debugging model behaviour.
type TracingAdapter struct {
	inner  LLMAdapter
	logger zerolog.Logger
	mu     sync.Mutex
	file   *os.File
	seq    int // monotonic call counter
}

// NewTracingAdapter wraps inner and logs every Complete call to path.
// The caller should defer Close on the returned adapter.
func NewTracingAdapter(inner LLMAdapter, path string) (*TracingAdapter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("llm trace: open %s: %w", path, err)
	}
	logger := zerolog.New(f).With().Timestamp().Str("model", inner.Model()).Logger()
	return &TracingAdapter{inner: inner, logger: logger, file: f}, nil
}

// Complete delegates to the wrapped adapter, logging the full prompt and
// response before returning.
func (t *TracingAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	t.mu.Lock()
	t.seq++
	seq := t.seq
	t.mu.Unlock()

	start := time.Now()

	// Log request
	var userParts []string
	for _, m := range msgs {
		userParts = append(userParts, fmt.Sprintf("[%s] %s", m.Role, m.Content))
	}

	t.logger.Info().
		Int("seq", seq).
		Bool("force_json", opts.ForceJSON).
		Str("system", system).
		Str("messages", strings.Join(userParts, "\n---\n")).
		Msg("LLM request")

	response, err := t.inner.Complete(ctx, system, msgs, opts, onChunk)
	elapsed := time.Since(start)

	if err != nil {
		t.logger.Error().
			Int("seq", seq).
			Dur("elapsed", elapsed).
			Err(err).
			Msg("LLM error")
		return "", err
	}

	t.logger.Info().
		Int("seq", seq).
		Dur("elapsed", elapsed).
		Int("response_len", len(response)).
		Str("response", response).
		Msg("LLM response")

	return response, nil
}

// Model delegates to the wrapped adapter.
func (t *TracingAdapter) Model() string { return t.inner.Model() }

// Close flushes and closes the trace log file.
func (t *TracingAdapter) Close() error { return t.file.Close() }
