package materialize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLensFactSet_MeasureSandwichBusyRate measures the N-mount settle
// sandwich (design 7.4: its busy rate grows with N and must be measured) on
// a 3-mount lens of synthetic facts while writers commit to the members:
// for each writer pace, the share of first calls answered ErrIndexMoved
// (busy) within the anchor wait, and the first calls' latency. CI never runs
// it (KNOMIT_LENS_FACTSET_MEASURE=1 to run; KNOMIT_LENS_FACTSET_MEASURE_N
// facts per member, default 300; KNOMIT_LENS_FACTSET_MEASURE_CALLS first
// calls per pace, default 20). The numbers describe this machine and this
// build, not a property of the code: nothing is derived from them.
func TestLensFactSet_MeasureSandwichBusyRate(t *testing.T) {
	if os.Getenv("KNOMIT_LENS_FACTSET_MEASURE") == "" {
		t.Skip("set KNOMIT_LENS_FACTSET_MEASURE=1 to measure")
	}
	n, calls := 300, 20
	if v, err := strconv.Atoi(os.Getenv("KNOMIT_LENS_FACTSET_MEASURE_N")); err == nil && v > 0 {
		n = v
	}
	if v, err := strconv.Atoi(os.Getenv("KNOMIT_LENS_FACTSET_MEASURE_CALLS")); err == nil && v > 0 {
		calls = v
	}
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	for k, f := range lt.members() {
		for i := 0; i < n; i++ {
			cos := 0.95 - 0.5*float64(i)/float64(n) - 0.001*float64(k)
			f.write(t, fmt.Sprintf("kb/u/%s-%03d.md", f.ri.Name(), i), "F"+strconv.Itoa(i), cosBody(cos, "synthetic."))
		}
	}
	wait := time.Duration(factsetAnchorWait.Load())
	t.Logf("3-mount lens, %d facts per member, anchor wait %s, %d first calls per pace", n, wait, calls)
	for _, pace := range []time.Duration{-1, 0, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond} {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		var writes atomic.Int64
		if pace >= 0 {
			// One writer per member, each committing one fact per pace (0:
			// back to back), so a sync is in flight on some mount most of the
			// time at the fastest paces.
			for _, f := range lt.members() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; ctx.Err() == nil; i++ {
						ff := f
						if _, err := ff.svc.Facts().WriteFact(context.Background(), "agent/test", fmt.Sprintf("kb/w/%s-%d.md", ff.ri.Name(), i),
							"---\ntype: observation\nconfidence: 0.7\n---\n# W\n\nw.\n", "w", ""); err == nil {
							writes.Add(1)
						}
						if pace > 0 {
							select {
							case <-ctx.Done():
							case <-time.After(pace):
							}
						}
					}
				}()
			}
		}
		var ok, busy, other int
		var lat []time.Duration
		for i := 0; i < calls; i++ {
			req := lt.request("anything", "kb/u/")
			req.Options.Depth = 1
			t0 := time.Now()
			p, _, err := FactSetFirstPage(context.Background(), lt.scope(), req)
			lat = append(lat, time.Since(t0))
			switch {
			case err == nil:
				ok++
				require.NoError(t, CloseFactSet(context.Background(), lt.scope(), *p.Cursor))
			case errors.Is(err, ErrIndexMoved):
				busy++
			default:
				other++
				t.Logf("error: %v", err)
			}
		}
		cancel()
		wg.Wait()
		slices.Sort(lat)
		label := "no writers"
		if pace == 0 {
			label = "writers back to back"
		} else if pace > 0 {
			label = "one write per " + pace.String() + " per member"
		}
		t.Logf("MEASURE %-36s busy %2d/%d (%.0f%%), ok %d, other %d; latency p50 %s p90 %s max %s; %d writes",
			label, busy, calls, 100*float64(busy)/float64(calls), ok, other,
			lat[len(lat)/2].Round(time.Millisecond), lat[len(lat)*9/10].Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond), writes.Load())
	}
}
