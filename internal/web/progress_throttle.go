package web

import "time"

// progressThrottle decides whether a long-job progress update (e.g. index
// rebuild) should be forwarded to the client. It always forwards the first and
// final (done>=total) updates, and rate-limits the ones in between to at most
// one per interval.
//
// This replaces a fixed `done%N==0` gate that, being misaligned with the
// 32-fact embeddings batch size, withheld every update until done=160 — so the
// connect/rebuild UI sat frozen at "0/total" for minutes on slow machines. A
// time interval is batch-size-agnostic: slow machines (batches far apart) see
// every batch, fast machines / huge corpora don't flood the stream.
type progressThrottle struct {
	interval time.Duration
	now      func() time.Time // injectable for tests
	last     time.Time
	started  bool // whether the first update has been forwarded
}

func newProgressThrottle(interval time.Duration) *progressThrottle {
	return &progressThrottle{interval: interval, now: time.Now}
}

// allow reports whether this (done,total) update should be emitted.
func (p *progressThrottle) allow(done, total int) bool {
	if total > 0 && done >= total {
		return true // completion always shows
	}
	now := p.now()
	if !p.started {
		// First update leaves the empty state immediately. Gated on a flag (not
		// done<=0) so a phase that repeatedly emits 0/0 — e.g. total unknown —
		// is rate-limited after the first one instead of forwarding every call.
		p.started = true
		p.last = now
		return true
	}
	if now.Sub(p.last) >= p.interval {
		p.last = now
		return true
	}
	return false
}
