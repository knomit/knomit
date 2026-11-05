package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
		`provider: exit status 1: 503 Service Unavailable`,
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

// TestWrapChat_CancelledParentSurfacesCancellation: a parent cancellation must
// reach the caller as one, even when the attempt failed for an unrelated
// reason. Returning only the provider's error erases the cancellation:
// errors.Is(err, context.Canceled) goes false, and a graceful shutdown reads as
// a provider outage to every caller branching on it. The cancellation paths
// need the parent context cancelled at a precise moment relative to an attempt,
// which no real transport can be asked for, hence the scripted fakeChat.
func TestWrapChat_CancelledParentSurfacesCancellation(t *testing.T) {
	t.Run("cancelled during the attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){
			func(context.Context, func(Event)) (*Response, error) {
				// Cancelled while in flight; the attempt then fails on its own terms.
				cancel()
				return nil, errors.New("ollama: HTTP 503: server overloaded")
			},
		}}

		_, err := WrapChat(f, Policy{MaxRetries: 2}).Stream(ctx, Snapshot{}, StreamOptions{}, nil)
		require.ErrorIs(t, err, context.Canceled, "the caller's cancellation must survive to the caller")
		require.Contains(t, err.Error(), "503", "the provider's failure must stay legible for logs")
		require.EqualValues(t, 1, f.attempts.Load(), "a cancelled request must not spend a retry")
	})

	t.Run("cancelled during the backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := &fakeChat{script: []func(context.Context, func(Event)) (*Response, error){
			func(context.Context, func(Event)) (*Response, error) {
				// Lands inside the backoff, which is at least retryBackoffBase/2.
				time.AfterFunc(20*time.Millisecond, cancel)
				return nil, errors.New("ollama: HTTP 503: server overloaded")
			},
		}}

		_, err := WrapChat(f, Policy{MaxRetries: 2}).Stream(ctx, Snapshot{}, StreamOptions{}, nil)
		require.ErrorIs(t, err, context.Canceled, "cancellation during backoff must surface as cancellation")
		require.Contains(t, err.Error(), "503")
		require.EqualValues(t, 1, f.attempts.Load(), "the pending retry must be abandoned, not run")
	})
}
