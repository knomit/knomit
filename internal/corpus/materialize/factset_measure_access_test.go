package materialize

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFactSet_MeasureAccess measures RANDOM ACCESS on one synthetic shape
// (same gates and store as TestFactSet_MeasureMatrix; CI never runs it): a far
// seek to the last unit, a backward seek to unit 3 after it, two readers
// alternating at units 3 and 400 of one FactSet, and 4 and 16 FactSets built
// and far-sought at once (wall time, heap peak, and how many calls were
// refused).
func TestFactSet_MeasureAccess(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE") == "" {
		t.Skip("set " + measureEnv + "=1, KNOMIT_FACTSET_MEASURE_DB and KNOMIT_FACTSET_MEASURE_SHAPE to measure")
	}
	name := os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE")
	f, _ := measureStore(t, false)
	ctx := context.Background()
	SetFactSetCacheBudget(0) // the production default
	defer SetFactSetCacheBudget(0)
	req := FactSetRequest{Binding: f.b, Facts: MaxFactSetFacts, Limit: HumanMaxLimit,
		Options: Options{Depth: MaxDepth, MaxNodes: MaxNodesLimit}}
	switch name {
	case "A", "B", "D":
		req.Search = FactSetSearch{Path: "kb/m4/" + string(rune(name[0]-'A'+'a')) + "/u/", Sort: SortRecent}
	default:
		t.Fatalf("unknown shape %q", name)
	}
	DropFactSetCacheForTest()
	p, _, err := FactSetFirstPage(ctx, f.scope("acc"), req)
	require.NoError(t, err)
	seek := func(unit int) (time.Duration, Stats) {
		t.Helper()
		t0 := time.Now()
		_, st, err := FactSetNextPageStats(ctx, f.scope("acc"), f.b, nil, nil, *p.Cursor, unit)
		require.NoError(t, err)
		return time.Since(t0), st
	}
	d, st := seek(p.Units)
	t.Logf("%s ACCESS far seek to %d: %s (%d units planned)", name, p.Units, d, st.UnitsPlanned)
	d, st = seek(3)
	t.Logf("%s ACCESS backward seek to 3 after it: %s (%d units planned)", name, d, st.UnitsPlanned)
	var total time.Duration
	planned := 0
	const rounds = 10
	for range rounds {
		for _, u := range []int{3, 400} {
			d, st := seek(u)
			total += d
			planned += st.UnitsPlanned
		}
	}
	t.Logf("%s ACCESS alternating readers at 3 / 400: %d calls, mean %s per call, %d units planned in all",
		name, 2*rounds, total/(2*rounds), planned)

	for _, n := range []int{4, 16} {
		DropFactSetCacheForTest()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		base := ms.HeapInuse
		stop := peakSampler()
		start := time.Now()
		var wg sync.WaitGroup
		errs := make([]error, n)
		lat := make([]time.Duration, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				owner := fmt.Sprintf("acc-%s-%d-%d", name, n, i)
				t0 := time.Now()
				p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
				if err == nil {
					_, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, p.Units)
				}
				errs[i], lat[i] = err, time.Since(t0)
			}()
		}
		wg.Wait()
		peak := stop()
		failed := 0
		var maxLat time.Duration
		for i, err := range errs {
			if err != nil {
				failed++
				t.Logf("%s ACCESS x%d call %d: %v", name, n, i, err)
			}
			maxLat = max(maxLat, lat[i])
		}
		t.Logf("%s ACCESS concurrent x%d FactSets (first page + far seek each): wall %s, slowest call %s, %d refused; heap peak %.1f MiB (before %.1f MiB); cache %.1f MiB accounted",
			name, n, time.Since(start), maxLat, failed, float64(peak)/(1<<20), float64(base)/(1<<20), float64(FactSetCacheBytes())/(1<<20))
	}
}
