package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"regexp"
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
	// attempt. The overall budget belongs to the caller's context, which
	// WithTimeout can only tighten, never extend. Note that this is where the
	// budget *should* live, not a guarantee that it does: a caller passing a
	// context with no deadline (as internal/synthesize currently does) is
	// bounded only by AttemptTimeout × (MaxRetries+1) plus backoff.
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
//   - ollama retries nothing internally, so the wrapper is the only resilience
//     it has.
//   - gemini is the one deliberate exception to the no-stacking rule above. It
//     does retry once inline, on cache-miss (gemini.go:177), so a single
//     logical request can reach 6 upstream calls. That is allowed because the
//     inline retry is not a retry of the same request — it rebuilds the config
//     without CachedContent, which this layer cannot do, having no idea a cache
//     was involved — and because its trigger is disjoint from this layer's:
//     isCacheMissingErr requires a "cached content … not found/expired" message,
//     which carries none of the statuses or phrases isOverloadedErr matches. In
//     practice the two never both fire, so the 6 is a bound, not a cost.
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
	//
	// The plain bool holds by construction, not by test: every adapter forwards
	// chunks from its own read loop (ollama.go:138, gemini.go:197, anthropic.go:62,
	// bedrock.go:95, the CLI adapters once at the end) and TracingAdapter passes
	// onChunk straight through. No test drives concurrent chunks, so -race has
	// nothing to prove here — an adapter that ever calls onChunk from another
	// goroutine would need this to become an atomic.Bool.
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
		//
		// The parent's error has to reach the caller, not just decide the retry:
		// an attempt that failed for an unrelated reason while the parent was
		// being cancelled would otherwise surface a graceful shutdown as a
		// provider outage, and callers branching on errors.Is(err,
		// context.Canceled) would take the wrong branch. Wrapping keeps both —
		// the cancellation for control flow, the provider detail for logs.
		if ctx.Err() != nil {
			return out, cancelledErr(ctx, err)
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
			return out, cancelledErr(ctx, err)
		}
	}
}

// cancelledErr reports the parent context's cancellation while keeping the
// attempt's own failure legible. errors.Is against context.Canceled and
// context.DeadlineExceeded must work on the result — that is the whole point —
// so the ctx error is the wrapped one and the provider error is rendered with
// %v rather than %w. Two errors.Is targets in one chain would let a caller
// asking "was this cancelled?" get a yes from a provider error that merely
// happened to wrap a context error of its own.
func cancelledErr(ctx context.Context, err error) error {
	return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), err)
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

// overloadStatusRe matches an overload status code — 429 rate limited, 503
// unavailable, 529 anthropic's overloaded — but only where a status code
// actually appears in the error strings this package sees: ollama formats its
// own as "ollama: HTTP %d:" (ollama.go:121), the genai SDK renders API errors
// as "Error %d, Message: …" (api_client.go:508), and JSON error envelopes
// spell it `"code": %d`.
//
// The anchor is the load-bearing part. A bare strings.Contains(msg, "429")
// retries `ollama: HTTP 400: {"error":"input length 14293 exceeds context
// window 8192"}` — the most common ollama 400 — and any request id that
// happens to contain the digits, each costing the full retry budget to fail
// identically. The trailing \b likewise keeps 4291 from reading as 429.
var overloadStatusRe = regexp.MustCompile(`(?:http(?:/1\.1)?|error|code"?:?|status"?:?) ?(?:429|503|529)\b`)

// overloadMarkers are phrases that only a provider refusing load produces.
// They are spelled out in full for the same reason: "rate limit" alone also
// appears in prompts, and our error strings quote request bodies back.
var overloadMarkers = []string{
	"overloaded",
	"rate limit exceeded",
	"rate_limit_error",
	"too many requests",
	"service unavailable",
	"resource_exhausted",
	"resource has been exhausted",
}

// isOverloadedErr reports whether err looks like the provider asking us to
// come back later. Providers surface these as opaque wrapped errors with no
// typed status reachable through errors.As, so matching has to be textual.
//
// Textual matching is also what isCacheMissingErr does (gemini.go:208), but the
// resemblance stops there and this function must not be relaxed to that
// function's shape: a cache-miss retry is one extra call on a request that was
// about to fail anyway, whereas a false positive here spends the whole retry
// budget — up to two more attempts of up to AttemptTimeout each — re-sending a
// request that is invalid and will fail identically every time. Hence anchored
// codes and full phrases rather than bare substrings.
func isOverloadedErr(err error) bool {
	msg := strings.ToLower(err.Error())
	if overloadStatusRe.MatchString(msg) {
		return true
	}
	for _, marker := range overloadMarkers {
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
