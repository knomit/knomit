package web

import "time"

// progressThrottle decides whether a long-job progress update (e.g. index
// rebuild) should be forwarded to the client. It always forwards the first
// (done<=0) and final (done>=total) updates, and rate-limits the ones in
// between to at most one per interval.
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
	if done <= 0 {
		p.last = now
		return true // first update leaves the empty state immediately
	}
	if now.Sub(p.last) >= p.interval {
		p.last = now
		return true
	}
	return false
}
