package sessions

import (
	"context"

	"github.com/ysmood/goob"
)

// Change is one "this row changed" ping, published after a successful write
// and carried to connected browsers by GET /api/v1/sessions/events.
//
// It is DELIBERATELY just an id and a kind. Nothing about the operator's
// machine travels on this stream, so the ReadOnly redaction that the list
// handler applies has nothing to duplicate here, and a consumer that wanted
// row detail has no choice but to re-read the list — which is the redacted,
// policy-carrying, single source of truth.
//
// Kinds: "touch" (a request bumped a row, inserting it on first sight),
// "init" (initialize declared client info), "end" (explicit DELETE),
// "purge" (retention deleted rows; ID is empty, since no single row is meant).
type Change struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// WithHub attaches a change hub whose lifetime is ctx, and returns the Store
// so the one construction site reads as a single expression. A Store without
// one is fully functional: publish is a no-op, which is what keeps every
// test and any caller that never wires a hub working unchanged.
func (s *Store) WithHub(ctx context.Context) *Store {
	s.ob = goob.New(ctx)
	return s
}

// Subscribe returns the stream of Changes until ctx ends.
//
// On a hubless Store it returns nil — a channel that blocks forever rather
// than one that is already closed. A closed channel would end the SSE
// handler's loop the instant it started, and the browser's EventSource would
// reconnect into the same instant failure, spinning. Blocking degrades the
// stream to keepalives instead: no events, no loop.
func (s *Store) Subscribe(ctx context.Context) goob.Events {
	if s.ob == nil {
		return nil
	}
	return s.ob.Subscribe(ctx)
}

// publish announces a change. Callers invoke it only AFTER the write it
// describes has succeeded, and only when that write actually changed
// something: a ping wakes every connected browser into a list re-read, so a
// ping for a no-op is a round trip that can never show anything new.
func (s *Store) publish(id, kind string) {
	if s.ob == nil {
		return
	}
	s.ob.Publish(Change{ID: id, Kind: kind})
}
