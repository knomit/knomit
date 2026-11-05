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
	// context with no deadline (as internal/corpus/synthesize currently does) is
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
//   - anthropic configures no retry settings of its own, so the SDK defaults
//     apply: anthropic-sdk-go retries transport, 429 and 5xx failures itself.
//     Retrying again out here would multiply the attempts a single logical
//     request makes against an already-overloaded provider. It still gets an
//     attempt timeout: the SDK's retries happen inside one attempt from this
//     layer's view, so the timeout is what bounds the whole stall.
//   - ollama retries nothing internally, so the wrapper is the only resilience
//     it has.
//
// A provider with no case here gets the default timeout and no retries. Bedrock
// and Gemini have no case because they have no adapter yet; their v2 adapters
// add one when they land.
func policyFor(provider string) Policy {
	p := Policy{AttemptTimeout: defaultAttemptTimeout}
	switch provider {
	case "anthropic":
		p.MaxRetries = 0
	case "ollama":
		p.MaxRetries = 2
	}
	return p
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
