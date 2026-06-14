package web

import (
	"testing"
	"time"
)

func TestProgressThrottle_ForwardsFirstFinalThrottlesMiddle(t *testing.T) {
	now := time.Unix(0, 0)
	th := newProgressThrottle(250 * time.Millisecond)
	th.now = func() time.Time { return now }

	if !th.allow(0, 100) {
		t.Fatal("first update (0/100) must be forwarded")
	}
	now = now.Add(10 * time.Millisecond)
	if th.allow(32, 100) {
		t.Error("mid update 10ms after the last must be throttled")
	}
	now = now.Add(10 * time.Millisecond)
	if th.allow(64, 100) {
		t.Error("mid update 20ms after the last must be throttled")
	}
	now = now.Add(300 * time.Millisecond)
	if !th.allow(96, 100) {
		t.Error("mid update after the interval elapsed must be forwarded")
	}
	now = now.Add(1 * time.Millisecond)
	if !th.allow(100, 100) {
		t.Error("final update (100/100) must always be forwarded")
	}
}

// TestProgressThrottle_SlowRebuildForwardsEveryBatch is the regression test for
// the frozen-"0/568" bug: when each rebuild batch is far apart in time (a slow
// machine), EVERY batch update must reach the UI. The old consumer gate
// (`done%10`/`done%20`), misaligned with the 32-fact batch size, forwarded only
// done∈{160,320,480,total} — so the UI sat at 0/total for minutes.
func TestProgressThrottle_SlowRebuildForwardsEveryBatch(t *testing.T) {
	now := time.Unix(0, 0)
	th := newProgressThrottle(250 * time.Millisecond)
	th.now = func() time.Time { return now }

	const total, batch = 568, 32
	forwarded := 0
	th.allow(0, total) // first
	for done := batch; done < total; done += batch {
		now = now.Add(2 * time.Second) // slow machine: ~2s per batch
		if th.allow(done, total) {
			forwarded++
		}
	}
	// 568/32 = 17 mid-batches before the final; on a slow machine all forward.
	if want := total/batch - 1; forwarded < want {
		t.Errorf("slow rebuild must forward ~every batch: forwarded=%d, want >=%d", forwarded, want)
	}
}
