package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// errAttemptStalled is the cancel cause when an attempt produced no event
// within Policy.AttemptTimeout.
var errAttemptStalled = errors.New("llm: no event within the attempt timeout")

// resilientChat is the v2 twin of resilientAdapter: bounded retries of
// retryable failures, a stall timeout, and request metrics.
type resilientChat struct {
	inner  ChatModel
	policy Policy
}

// WrapChat applies p to inner. It reuses the v1 classifier (isRetryable), the
// backoff (retryBackoff) and, through callers, the per-provider budgets
// (policyFor).
//
// Two semantics differ from the v1 wrapper because a v2 request streams:
//
//   - A retry happens only while nothing has reached the sink. Once an event
//     has been delivered, a replay would deliver it again, so the error is
//     returned as is.
//   - Policy.AttemptTimeout bounds the time to the FIRST event, not the whole
//     stream. A chat turn that streams for ten minutes is healthy; one that
//     has said nothing for AttemptTimeout is the stall this exists to catch.
//     The overall budget belongs to the caller's context.
//
// The result is a ChatModel, not a concrete type, and every ChatModel method
// delegates, so wrapping never changes Caps (the decorator-erasure trap of
// the v1 BatchAdapter cannot recur as long as v2 has no optional interfaces).
func WrapChat(inner ChatModel, p Policy) ChatModel {
	return &resilientChat{inner: inner, policy: p}
}

func (r *resilientChat) Provider() string { return r.inner.Provider() }
func (r *resilientChat) Model() string    { return r.inner.Model() }
func (r *resilientChat) Caps() Caps       { return r.inner.Caps() }

// Stream runs inner under the policy. observeLLMRequest records the whole
// logical request (all attempts and backoff), matching the histogram's
// documented meaning in metrics.go.
func (r *resilientChat) Stream(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event)) (*Response, error) {
	defer observeLLMRequest(time.Now())
	if sink == nil {
		sink = func(Event) {}
	}
	var emitted atomic.Bool
	for attempt := 0; ; attempt++ {
		resp, err := r.attempt(ctx, s, o, sink, &emitted)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, cancelledErr(ctx, err)
		}
		if emitted.Load() || attempt >= r.policy.MaxRetries || !isRetryable(err) {
			return nil, err
		}
		llmRetriesTotal.Inc()
		backoff := retryBackoff(attempt)
		log.Warn().Err(err).
			Str("provider", r.inner.Provider()).
			Str("model", r.inner.Model()).
			Int("attempt", attempt+1).
			Dur("backoff", backoff).
			Msg("llm: retrying stream after retryable failure")
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, cancelledErr(ctx, err)
		}
	}
}

// attempt runs one inner Stream with the first-event timer armed.
func (r *resilientChat) attempt(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event), emitted *atomic.Bool) (*Response, error) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var timer *time.Timer
	if r.policy.AttemptTimeout > 0 {
		timer = time.AfterFunc(r.policy.AttemptTimeout, func() {
			// Stop on the first event normally prevents this from running;
			// the check covers the race where it fires as that event lands.
			if !emitted.Load() {
				cancel(errAttemptStalled)
			}
		})
		defer timer.Stop()
	}
	resp, err := r.inner.Stream(actx, s, o, func(e Event) {
		if !emitted.Swap(true) && timer != nil {
			timer.Stop()
		}
		sink(e)
	})
	if err != nil && errors.Is(context.Cause(actx), errAttemptStalled) && ctx.Err() == nil {
		// Report the stall as a deadline, which isRetryable treats as
		// retryable, keeping the provider's own error for the log.
		return nil, fmt.Errorf("%w: %w (provider: %v)", context.DeadlineExceeded, errAttemptStalled, err)
	}
	return resp, err
}
