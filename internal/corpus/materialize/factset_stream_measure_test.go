package materialize

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type noFiles struct{}

func (noFiles) OpenFactSetFiles() int { return 0 }

// TestStream_MeasureMatrix measures the streaming prototype on the round-4
// stores, one shape per process (the first pass cold, the second warm), and
// optionally 4 concurrent builds. Env-gated like every measurement here
// (KNOMIT_FACTSET_MEASURE, _DB, _BRANCH, _SHAPE, _PROFILE, _CONCURRENT), so
// CI never runs it.
func TestStream_MeasureMatrix(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE") == "" {
		t.Skip("set " + measureEnv + "=1, KNOMIT_FACTSET_MEASURE_DB and KNOMIT_FACTSET_MEASURE_SHAPE to measure")
	}
	name := os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE")
	shape, ok := measureShapes[name]
	require.True(t, ok, name)
	f, _ := measureStore(t, false)
	req := FactSetRequest{Binding: f.b, Search: FactSetSearch{Path: shape.prefix, Sort: SortRelevance},
		Facts: MaxFactSetFacts, Limit: HumanMaxLimit,
		Options: Options{Depth: MaxDepth, MaxNodes: shape.maxNodes}}
	ctx := context.Background()
	prof := func(label string) func() {
		dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE")
		if dir == "" {
			return func() {}
		}
		pf, err := os.Create(filepath.Join(dir, "stream-"+label+"-"+name+".pprof"))
		require.NoError(t, err)
		require.NoError(t, pprof.StartCPUProfile(pf))
		return func() { pprof.StopCPUProfile(); _ = pf.Close() }
	}
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	t.Logf("SHAPE %s baseline heap in use %d MiB", name, base.HeapInuse>>20)

	for pass, label := range []string{"cold", "warm"} {
		owner := fmt.Sprintf("s-%s-%d", name, pass)
		runtime.GC()
		stop := peakSampler(noFiles{})
		var endProf func()
		if pass == 0 {
			endProf = prof("page1")
		}
		t1 := time.Now()
		s, err := CreateStream(ctx, f.scope(owner), req)
		require.NoError(t, err)
		created := time.Since(t1)
		page, _, _, err := s.Page(ctx, 1, 0, req.Limit)
		require.NoError(t, err)
		first := time.Since(t1)
		if endProf != nil {
			endProf()
		}
		st1 := s.Stats
		wr1, rs1 := s.MemoryEntries()

		// Far seek: the cache is gone (restart), the state is rebuilt from the
		// one session row and replayed without bodies to unit 500.
		DropStreamCacheForTest()
		if pass == 0 {
			endProf = prof("seek")
		}
		t2 := time.Now()
		s2, err := OpenStream(ctx, f.scope(owner), f.b, nil, nil, s.ID)
		require.NoError(t, err)
		far, _, _, err := s2.Page(ctx, s2.Units(), 0, req.Limit)
		require.NoError(t, err)
		seek := time.Since(t2)
		if pass == 0 {
			endProf()
		}
		st2 := s2.Stats
		wr2, rs2 := s2.MemoryEntries()

		t3 := time.Now()
		var n countWriter
		enc := json.NewEncoder(&n)
		require.NoError(t, s2.Export(ctx, func(sec StreamSection) error { return enc.Encode(sec) }))
		export := time.Since(t3)
		heap, _ := stop()
		st3 := s2.Stats
		t.Logf("SHAPE %s %s: units %d; page 1 %s (create+reserve %s; %d sections; queries %d, object reads %d, session writes %d, per-fact ops %d; reserved %d, written %d); "+
			"far seek %s (to unit %d, %d sections; units walked %d; queries %d, object reads %d, session writes %d, per-fact ops %d; written %d, reserved %d); "+
			"export %s (%d MiB, %d sections; queries %d, object reads %d); heap peak %d MiB (baseline %d MiB)",
			name, label, s.Units(), first, created, len(page), st1.IndexQueries, st1.ObjectReads, st1.SessionWrites, st1.PerFactOps, rs1, wr1,
			seek, s2.Units(), len(far), st2.UnitsWalked, st2.IndexQueries, st2.ObjectReads, st2.SessionWrites, st2.PerFactOps, wr2, rs2,
			export, n.n>>20, st3.Sections-st2.Sections, st3.IndexQueries-st2.IndexQueries, st3.ObjectReads-st2.ObjectReads,
			heap>>20, base.HeapInuse>>20)
		DropStreamCacheForTest()
	}

	if os.Getenv("KNOMIT_FACTSET_MEASURE_CONCURRENT") == "" {
		return
	}
	runtime.GC()
	stop := peakSampler(noFiles{})
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	walls := make([]time.Duration, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			s, err := CreateStream(ctx, f.scope(fmt.Sprintf("s-%s-c%d", name, i)), req)
			if err == nil {
				_, _, _, err = s.Page(ctx, 1, 0, req.Limit)
			}
			if err == nil {
				_, _, _, err = s.Page(ctx, s.Units(), 0, req.Limit)
			}
			errs[i], walls[i] = err, time.Since(t0)
		}()
	}
	wg.Wait()
	heap, _ := stop()
	for _, err := range errs {
		require.NoError(t, err)
	}
	t.Logf("SHAPE %s concurrent x4 (create + page 1 + far seek each): wall %s; per build %v; heap peak %d MiB",
		name, time.Since(start), walls, heap>>20)
}
