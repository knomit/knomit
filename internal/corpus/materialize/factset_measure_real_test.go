package materialize

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/storetest"
)

// THE MEASUREMENT MATRIX of the streaming FactSet (PR C plan section 7),
// ported from the prototype's harness. Env-gated: CI never runs it.
//
//   KNOMIT_FACTSET_MEASURE=1        the gate
//   KNOMIT_FACTSET_MEASURE_DB       the store file (a COPY: opening a store
//                                   may write to it)
//   KNOMIT_FACTSET_MEASURE_BRANCH   its branch (default agent/test)
//   KNOMIT_FACTSET_MEASURE_SEED=1   TestFactSet_MeasureSeed seeds shapes A and
//                                   D into it (creating the store if absent)
//   KNOMIT_FACTSET_MEASURE_SHAPE    A, B or D (synthetic: 500 results at
//                                   Depth 10, References 499), or REAL (a real
//                                   store: 50 results, the defaults)
//   KNOMIT_FACTSET_MEASURE_PREFIX   REAL only: the search path (default kb/)
//   KNOMIT_FACTSET_MEASURE_UNITS    results per seeded shape (default 500)
//
// Shapes, under kb/m4/<shape>/ (bodies ~2.5 KiB):
//   A shared: every result cites the same 499 leaves;
//   B distinct: every result cites 499 facts of its own (the existing
//     synth/b3.db store, seeded by the prototype's harness at the same
//     paths);
//   D deep: every result cites 50 chains of 10 facts of its own, so a unit
//     reaches Depth 10 and its 500-node cap through depth.

const measureEnv = "KNOMIT_FACTSET_MEASURE"

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

const measureBody = `This fact records an observation made while building the materialize
FactSet measurements. It is written to look like a real fact: a few
paragraphs of prose with caveats, numbers and references, so that the
store's rows, its blob objects and its page cache behave like production.
`

// uname is a uuid-like file name for a seeded fact (real facts are named by
// server-generated uuids).
func uname(parts ...any) string {
	h := fnv.New64a()
	fmt.Fprint(h, parts...)
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

func realisticBody(seed string) string {
	var sb strings.Builder
	for sb.Len() < 2500 {
		sb.WriteString(measureBody)
		sb.WriteString("Seed " + seed + ": the walk reads index rows and edges, never a body.\n\n")
	}
	return sb.String()
}

func measureFactText(t testing.TB, path, title string, refs []string) string {
	ff := fact.NewFact(path)
	ff.Title, ff.Body, ff.Type = title, realisticBody(path), fact.Observation
	ff.Domain, ff.Entities, ff.Refs = []string{"measure"}, []string{"FactSet"}, refs
	ff.Confidence, ff.Sources = 0.8, 1
	content, err := fact.SerializeFact(ff)
	require.NoError(t, err)
	return content
}

func measureStore(t testing.TB, create bool) (*factsetFixture, string) {
	path := os.Getenv("KNOMIT_FACTSET_MEASURE_DB")
	require.NotEmpty(t, path, "KNOMIT_FACTSET_MEASURE_DB")
	branch := os.Getenv("KNOMIT_FACTSET_MEASURE_BRANCH")
	if branch == "" {
		branch = "agent/test"
	}
	_, statErr := os.Stat(path)
	svc, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	if os.IsNotExist(statErr) && create {
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))
	} else {
		require.NoError(t, svc.OpenRepo())
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "m4", UID: "uid-m4", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	return &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, branch)}, branch
}

func batchWrite(t testing.TB, f *factsetFixture, branch string, files map[string]string) {
	if len(files) == 0 {
		return
	}
	_, _, err := f.svc.Facts().BatchWriteFacts(context.Background(), branch, files, nil, "measure seed", "learn")
	require.NoError(t, err)
}

// TestFactSet_MeasureSeed seeds shapes A and D into KNOMIT_FACTSET_MEASURE_DB
// (B is the prototype's synth store, same paths). Batches stay near 500 facts:
// a commit's index sync costs more than linearly in the batch size.
func TestFactSet_MeasureSeed(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SEED") == "" {
		t.Skip("set " + measureEnv + "=1 and KNOMIT_FACTSET_MEASURE_SEED=1 to seed")
	}
	f, branch := measureStore(t, true)
	units := envInt("KNOMIT_FACTSET_MEASURE_UNITS", 500)
	start := time.Now()
	files := map[string]string{}
	leaves := make([]string, 499)
	for j := range leaves {
		leaves[j] = fmt.Sprintf("kb/m4/a/z/g%d/%s.md", j/50, uname("a", j))
		files[leaves[j]] = measureFactText(t, leaves[j], fmt.Sprintf("A leaf %d", j), nil)
	}
	batchWrite(t, f, branch, files)
	for lo := 0; lo < units; lo += 100 {
		files = map[string]string{}
		for i := lo; i < min(units, lo+100); i++ {
			p := fmt.Sprintf("kb/m4/a/u/g%02d/r%03d.md", i/25, i)
			files[p] = measureFactText(t, p, fmt.Sprintf("A result %d", i), leaves)
		}
		batchWrite(t, f, branch, files)
	}
	t.Logf("seed A: %s", time.Since(start))

	start = time.Now()
	const chunkD = 10 // 500 chain facts per batch
	for lo := 0; lo < units; lo += chunkD {
		for k := 9; k >= 0; k-- {
			files = map[string]string{}
			for i := lo; i < min(units, lo+chunkD); i++ {
				for j := range 50 {
					p := fmt.Sprintf("kb/m4/d/d/g%02d/r%03d/s%d/%s.md", i/25, i, j/5, uname("d", i, j, k))
					var refs []string
					if k < 9 {
						refs = []string{fmt.Sprintf("kb/m4/d/d/g%02d/r%03d/s%d/%s.md", i/25, i, j/5, uname("d", i, j, k+1))}
					}
					files[p] = measureFactText(t, p, fmt.Sprintf("D %d.%d.%d", i, j, k), refs)
				}
			}
			batchWrite(t, f, branch, files)
		}
		results := map[string]string{}
		for i := lo; i < min(units, lo+chunkD); i++ {
			heads := make([]string, 50)
			for j := range heads {
				heads[j] = fmt.Sprintf("kb/m4/d/d/g%02d/r%03d/s%d/%s.md", i/25, i, j/5, uname("d", i, j, 0))
			}
			p := fmt.Sprintf("kb/m4/d/u/g%02d/r%03d.md", i/25, i)
			results[p] = measureFactText(t, p, fmt.Sprintf("D result %d", i), heads)
		}
		batchWrite(t, f, branch, results)
		if done := min(units, lo+chunkD); done%50 == 0 || done == units {
			t.Logf("seed D: %d results in %s", done, time.Since(start))
		}
	}
}

// peakSampler samples the Go heap in use every 2 ms until stop.
func peakSampler() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() uint64 { close(done); wg.Wait(); return peak.Load() }
}

func heapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// TestFactSet_MeasureMatrix measures one shape on its store: page 1 (search
// time apart), the next page warm, the far seek to the last unit warm, the
// replay after eviction at the last unit, the full export, the cache entry at
// the maximum (accounted and heap), 4 concurrent walks (heap peak), the
// session-DB writes, and the per-path git reads (must be 0).
func TestFactSet_MeasureMatrix(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE") == "" {
		t.Skip("set " + measureEnv + "=1, KNOMIT_FACTSET_MEASURE_DB and KNOMIT_FACTSET_MEASURE_SHAPE to measure")
	}
	name := os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE")
	f, branch := measureStore(t, false)
	ti := f.svc.ToolSession()
	ctx := context.Background()
	SetFactSetCacheBudget(1 << 30) // measure the entry, not the eviction
	var gitReads atomic.Int64
	defer storetest.SetGitPathReadHook(func(string) { gitReads.Add(1) })()

	req := FactSetRequest{Binding: f.b, Facts: MaxFactSetFacts, Limit: HumanMaxLimit,
		Options: Options{Depth: MaxDepth, MaxNodes: MaxNodesLimit}}
	switch name {
	case "A":
		req.Search = FactSetSearch{Path: "kb/m4/a/u/", Sort: SortRecent}
	case "B":
		req.Search = FactSetSearch{Path: "kb/m4/b/u/", Sort: SortRecent}
	case "D":
		req.Search = FactSetSearch{Path: "kb/m4/d/u/", Sort: SortRecent}
	case "REAL":
		prefix := os.Getenv("KNOMIT_FACTSET_MEASURE_PREFIX")
		if prefix == "" {
			prefix = "kb/"
		}
		req.Search = FactSetSearch{Path: prefix, Sort: SortRecent}
		req.Facts, req.Options, req.Limit = 50, DefaultOptions(), HumanDefaultLimit
	default:
		t.Fatalf("unknown shape %q", name)
	}
	t.Logf("SHAPE %s on %s (%s), GOMAXPROCS %d, %s/%s", name, os.Getenv("KNOMIT_FACTSET_MEASURE_DB"), branch,
		runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)

	writes0 := ti.FactSetWrites()
	for pass, label := range []string{"cold", "warm"} {
		owner := fmt.Sprintf("m-%s-%d", name, pass)
		DropFactSetCacheForTest()
		base := heapAlloc()
		t1 := time.Now()
		page, st1, err := FactSetFirstPage(ctx, f.scope(owner), req)
		first := time.Since(t1)
		require.NoError(t, err)
		require.NotNil(t, page.Cursor, "a non-empty FactSet")
		t2 := time.Now()
		p2, _, err := FactSetNextPageStats(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, 0)
		next := time.Since(t2)
		require.NoError(t, err)
		_ = p2
		t.Logf("%s %s: units %d; page 1 %s (search %s, planning+page %s; %d index reads, %d of them parsing row-less versions, %d units planned, max %d reads per unit); next page %s",
			name, label, page.Units, first, st1.Search, first-st1.Search, st1.IndexQueries, st1.ParsedReads, st1.UnitsPlanned, st1.MaxUnitQueries, next)
		if name == "REAL" {
			t5 := time.Now()
			var n countWriter
			require.NoError(t, FactSetExport(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, func(FactSetEnv) io.Writer { return &n }))
			t.Logf("%s %s: export %s (%d KiB)", name, label, time.Since(t5), n.n>>10)
			continue
		}
		var prof *os.File
		if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" && pass == 1 {
			prof, err = os.Create(dir + "/seek-" + name + ".pprof")
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(prof))
		}
		t3 := time.Now()
		ps, st3, err := FactSetNextPageStats(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, page.Units)
		seek := time.Since(t3)
		if prof != nil {
			pprof.StopCPUProfile()
			_ = prof.Close()
		}
		require.NoError(t, err)
		_, cached := FactSetCachedForTest(*page.Cursor)
		held := heapAlloc() - base
		DropFactSetCacheForTest()
		if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" && pass == 1 {
			prof, err = os.Create(dir + "/replay-" + name + ".pprof")
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(prof))
		}
		t4 := time.Now()
		_, st4, err := FactSetNextPageStats(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, page.Units)
		replay := time.Since(t4)
		if prof != nil {
			pprof.StopCPUProfile()
			_ = prof.Close()
			prof = nil
		}
		require.NoError(t, err)
		t5 := time.Now()
		var n countWriter
		require.NoError(t, FactSetExport(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, func(FactSetEnv) io.Writer { return &n }))
		export := time.Since(t5)
		t.Logf("%s %s: far seek to unit %d %s (%d units planned, %d sections planned in all); replay after eviction %s (%d cache miss, %d units planned); export %s (%d MiB); cache entry accounted %.1f MiB, heap held %.1f MiB",
			name, label, page.Units, seek, st3.UnitsPlanned, ps.Sections, replay, st4.CacheMisses, st4.UnitsPlanned, export, n.n>>20,
			float64(cached)/(1<<20), float64(held)/(1<<20))
	}
	t.Logf("%s: session-DB writes %d for %d FactSets (heartbeats included); per-path git reads %d", name, ti.FactSetWrites()-writes0, 2, gitReads.Load())

	if name == "REAL" {
		return
	}
	DropFactSetCacheForTest()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	baseInuse := ms.HeapInuse
	stop := peakSampler()
	start := time.Now()
	if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" {
		go func() {
			time.Sleep(time.Duration(envInt("KNOMIT_FACTSET_MEASURE_HEAP_AT_MS", 2500)) * time.Millisecond)
			if hf, err := os.Create(dir + "/heap-" + name + ".pprof"); err == nil {
				runtime.GC()
				_ = pprof.WriteHeapProfile(hf)
				_ = hf.Close()
			}
		}()
	}
	var wg sync.WaitGroup
	errs := make([]error, MaxConcurrentFactSetBuilds)
	for i := range MaxConcurrentFactSetBuilds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("m-%s-c%d", name, i)
			p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
			if err == nil {
				_, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, p.Units)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	peak := stop()
	for _, err := range errs {
		require.NoError(t, err)
	}
	t.Logf("%s concurrent x%d (first page + far seek each): wall %s; heap peak %.1f MiB (in use before: %.1f MiB); cache %.1f MiB accounted",
		name, MaxConcurrentFactSetBuilds, time.Since(start), float64(peak)/(1<<20), float64(baseInuse)/(1<<20), float64(FactSetCacheBytes())/(1<<20))
	require.Zero(t, gitReads.Load(), "no per-path git read")
}
