package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// errAttemptStalled is the cancel cause when an attempt showed no activity
// (no sink event and no StreamOptions.Progress call) within
// Policy.AttemptTimeout.
var errAttemptStalled = errors.New("llm: no event within the attempt timeout")

// resilientChat decorates a ChatModel with bounded retries of retryable
// failures, a stall timeout, and request metrics.
type resilientChat struct {
	inner  ChatModel
	policy Policy
}

// WrapChat applies p to inner. It uses the classifier (isRetryable), the
// backoff (retryBackoff) and, through callers, the per-provider budgets
// (policyFor).
//
// A request streams, so:
//
//   - A retry happens only while nothing has reached the sink. Once an event
//     has been delivered, a replay would deliver it again, so the error is
//     returned as is.
//   - Policy.AttemptTimeout bounds the time to the FIRST sign of life, not
//     the whole stream. A sign of life is a sink event OR a
//     StreamOptions.Progress call, which adapters make for every provider
//     stream event: thinking, signature and tool-argument deltas reach no
//     sink but do count, so a model that thinks for minutes before its first
//     text is not cut off. A chat turn that streams for ten minutes is
//     healthy; one whose provider has sent nothing for AttemptTimeout is the
//     stall this exists to catch. The overall budget belongs to the caller's
//     context. Progress alone does not forbid a retry: only a delivered sink
//     event does, since only that would be delivered twice.
//
// The result is a ChatModel, not a concrete type, and every ChatModel method
// delegates, so wrapping never changes Caps. A decorator that returns a
// concrete type can drop an optional interface its inner model satisfied,
// with no error and no compile failure; ChatModel has no optional interfaces,
// and TestWrapChat_KeepsCaps pins that Caps survive the wrap.
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

// attempt runs one inner Stream with the first-activity timer armed.
func (r *resilientChat) attempt(ctx context.Context, s Snapshot, o StreamOptions, sink func(Event), emitted *atomic.Bool) (*Response, error) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		timer *time.Timer
		alive atomic.Bool
	)
	if r.policy.AttemptTimeout > 0 {
		timer = time.AfterFunc(r.policy.AttemptTimeout, func() {
			// Stop on the first activity normally prevents this from running;
			// the check covers the race where it fires as that activity lands.
			if !alive.Load() {
				cancel(errAttemptStalled)
			}
		})
		defer timer.Stop()
	}
	markAlive := func() {
		if !alive.Swap(true) && timer != nil {
			timer.Stop()
		}
	}
	callerProgress := o.Progress
	o.Progress = func() {
		markAlive()
		if callerProgress != nil {
			callerProgress()
		}
	}
	resp, err := r.inner.Stream(actx, s, o, func(e Event) {
		markAlive()
		emitted.Store(true)
		sink(e)
	})
	if err != nil && errors.Is(context.Cause(actx), errAttemptStalled) && ctx.Err() == nil {
		// Report the stall as a deadline, which isRetryable treats as
		// retryable, keeping the provider's own error for the log.
		return nil, fmt.Errorf("%w: %w (provider: %v)", context.DeadlineExceeded, errAttemptStalled, err)
	}
	return resp, err
}
