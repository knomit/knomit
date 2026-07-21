package llm

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
)

// Policy is the resilience budget applied to one provider. It is intentionally
// small: the failures worth handling generically are "the attempt stalled" and
// "the provider is momentarily overloaded", and both are described by these two
// numbers.
type Policy struct {
	// AttemptTimeout bounds a single attempt (0 disables it). It is per-attempt
	// rather than per-request because the failure it exists to catch — a
	// connection that accepts and then never answers — is a property of one
	// attempt. The overall budget already has an owner: the caller's context,
	// which WithTimeout can only tighten, never extend.
	AttemptTimeout time.Duration
	// MaxRetries is the number of retries *after* the first attempt. Providers
	// whose SDKs already retry get 0 here; stacking would multiply attempts
	// against a provider that is, by hypothesis, already struggling.
	MaxRetries int
}

const (
	// defaultAttemptTimeout is deliberately generous. Synthesis completions over
	// a large fact corpus legitimately run for minutes, so a timeout tuned for
	// interactive latency would turn healthy long requests into failures — a far
	// worse outcome than tolerating a stall for a few extra minutes.
	defaultAttemptTimeout = 5 * time.Minute

	retryBackoffBase = 500 * time.Millisecond
	retryBackoffMax  = 8 * time.Second
)

// policyFor returns the retry/timeout budget for a provider. The asymmetry in
// MaxRetries is the whole point of the table:
//
//   - anthropic and bedrock configure no retry settings of their own, which
//     means their SDK defaults apply — anthropic-sdk-go retries transport,
//     429 and 5xx failures itself, and the AWS standard retryer does the same
//     for bedrock. Retrying again out here would multiply the attempts a single
//     logical request makes against an already-overloaded provider. They still
//     get an attempt timeout: the SDK's retries happen inside one attempt from
//     this layer's view, so the timeout is what bounds the whole stall.
//   - gemini and ollama retry nothing internally, so the wrapper is the only
//     resilience they have.
//   - the CLI adapters shell out to a subprocess. One retry covers transient
//     startup flake; more would mostly re-pay a slow process launch.
func policyFor(provider string) Policy {
	p := Policy{AttemptTimeout: defaultAttemptTimeout}
	switch provider {
	case "anthropic", "bedrock":
		p.MaxRetries = 0
	case "gemini", "ollama":
		p.MaxRetries = 2
	case "claudecli", "geminicli":
		p.MaxRetries = 1
	}
	return p
}

// resilientAdapter decorates a provider with a per-attempt timeout, bounded
// retries and request metrics.
type resilientAdapter struct {
	inner  LLMAdapter
	policy Policy
}

// resilientBatchAdapter exists only so that wrapping a batch-capable provider
// does not erase BatchAdapter from the resulting type. That loss would be
// silent: consumers discover batch support with a type assertion, which simply
// starts returning false, with no compile error anywhere.
type resilientBatchAdapter struct {
	*resilientAdapter
	batch BatchAdapter
}

// wrapResilient applies policy to inner, preserving BatchAdapter when inner
// implements it.
func wrapResilient(inner LLMAdapter, policy Policy) LLMAdapter {
	r := &resilientAdapter{inner: inner, policy: policy}
	if ba, ok := inner.(BatchAdapter); ok {
		return &resilientBatchAdapter{resilientAdapter: r, batch: ba}
	}
	return r
}

// Complete runs the wrapped provider under the policy, retrying only failures
// that are both retryable and invisible to the caller (see shouldRetry).
func (a *resilientAdapter) Complete(ctx context.Context, system string, msgs []Message, opts CompletionOptions, onChunk func(string)) (string, error) {
	defer observeLLMRequest(time.Now())

	// emitted is the gate that makes retrying safe. Every provider calls onChunk
	// synchronously from within Complete, on this goroutine, so a plain bool is
	// sufficient — and once it is set, replaying the request would deliver the
	// already-seen chunks a second time. That is the same reason gemini's
	// cache-miss retry checks `accumulated == ""` (gemini.go:177). Callers that
	// pass a nil onChunk observe nothing until Complete returns, so for them the
	// gate never closes and any retryable failure can be retried freely.
	emitted := false
	wrapped := onChunk
	if onChunk != nil {
		wrapped = func(s string) {
			emitted = true
			onChunk(s)
		}
	}

	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := a.attemptContext(ctx)
		out, err := a.inner.Complete(attemptCtx, system, msgs, opts, wrapped)
		cancel()
		if err == nil {
			return out, nil
		}

		// Disambiguate whose deadline fired. A cancelled *parent* means the
		// caller walked away — nothing left to retry for. Only the attempt
		// context expiring counts as a stall, and the attempt context is already
		// gone by now, so the parent is the only thing worth asking.
		if ctx.Err() != nil {
			return out, err
		}
		if emitted || attempt >= a.policy.MaxRetries || !isRetryable(err) {
			return out, err
		}

		llmRetriesTotal.Inc()
		backoff := retryBackoff(attempt)
		log.Warn().
			Err(err).
			Str("model", a.inner.Model()).
			Int("attempt", attempt+1).
			Dur("backoff", backoff).
			Msg("llm: retrying after retryable failure")

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return out, err
		}
	}
}

// attemptContext derives the per-attempt deadline. WithTimeout only tightens,
// so a caller that set a shorter deadline still wins.
func (a *resilientAdapter) attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.policy.AttemptTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, a.policy.AttemptTimeout)
}

// Model delegates to the wrapped adapter.
func (a *resilientAdapter) Model() string { return a.inner.Model() }

// BatchEnabled delegates to the wrapped adapter.
func (a *resilientBatchAdapter) BatchEnabled() bool { return a.batch.BatchEnabled() }

// CompleteBatch delegates with metrics only — no retry and no attempt timeout.
// A batch job is a long poll over a server-side lifecycle measured in minutes
// to hours, so an attempt timeout tuned for a completion would abort healthy
// work; and a failure mid-submit tells this layer nothing about whether the
// job was created, so replaying it risks duplicating a whole batch. The caller
// owns the batch deadline through ctx, which is the only place the necessary
// knowledge lives.
func (a *resilientBatchAdapter) CompleteBatch(ctx context.Context, requests []BatchRequest, opts CompletionOptions) ([]string, error) {
	defer observeLLMRequest(time.Now())
	return a.batch.CompleteBatch(ctx, requests, opts)
}

// isRetryable reports whether err is worth a second attempt. The default is
// no: an auth failure, a malformed request or a validation error will fail
// identically however many times it is sent, and retrying it only delays the
// error the caller needs to see.
func isRetryable(err error) bool {
	// The caller's own cancellation reaches here as context.Canceled; it is
	// never retryable. context.DeadlineExceeded, by contrast, is the attempt
	// timeout firing — Complete checks the parent context before consulting
	// this function, so a parent deadline has already been ruled out.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Transport failures say nothing about the request itself, only about the
	// connection that carried it.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	return isOverloadedErr(err)
}

// isOverloadedErr reports whether err looks like the provider asking us to
// come back later. Providers surface these as opaque wrapped errors with no
// typed status reachable through errors.As, so matching is textual — the same
// trade the repo already accepts in isCacheMissingErr (gemini.go:204), and for
// the same reason: a false positive costs one extra attempt, while a false
// negative merely preserves today's fail-immediately behaviour.
func isOverloadedErr(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"429", "529", "503", "overloaded", "rate limit"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// retryBackoff returns the pause before the retry following attempt n
// (0-based): exponential, capped, and half-jittered so that a fleet of workers
// tripped by the same provider outage does not resynchronise into a thundering
// herd on every subsequent attempt.
func retryBackoff(attempt int) time.Duration {
	d := retryBackoffBase << attempt
	if d > retryBackoffMax || d <= 0 {
		d = retryBackoffMax
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)))
}
