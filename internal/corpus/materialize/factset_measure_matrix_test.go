package materialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/sessions"
)

// THE WORST-CASE MATRIX (round 4 of PR #692). Env-gated by
// KNOMIT_FACTSET_MEASURE like every measurement here, so CI never runs it.
//
//   KNOMIT_FACTSET_MEASURE_DB      the store file (seeded once, measured many
//                                  times; a copy of a real store works)
//   KNOMIT_FACTSET_MEASURE_BRANCH  its branch (default agent/test)
//   KNOMIT_FACTSET_MEASURE_SEED=1  TestFactSet_MeasureSeed seeds the shapes
//                                  into it (creating the store if absent)
//   KNOMIT_FACTSET_MEASURE_SHAPE   A, B, C or D: TestFactSet_MeasureMatrix
//                                  measures one shape (run each shape in its
//                                  own process: the first pass is cold)
//   KNOMIT_FACTSET_MEASURE_PROFILE a directory: CPU profile of page 1
//   KNOMIT_FACTSET_MEASURE_UNITS   results per shape (default 500)
//   KNOMIT_FACTSET_MEASURE_CONCURRENT=1  also run 4 concurrent builds
//
// Shapes, under kb/m4/<shape>/ (bodies ~2.5 KiB):
//   A shared: every result cites the same 499 leaves (round 3's case);
//   B distinct: every result cites 499 facts of its own;
//   C deep: every result cites 50 chains of 10 facts of its own, so a unit
//     reaches Depth 10 and its 500-section cap through depth;
//   D default UI: B's data at References 199 (max_nodes 200), 100,000
//     sections, exactly the section cap.
// A, B and C run at References 499 (max_nodes 500), Depth 10, Facts 500,
// at the shipped 100,000-section cap.
//
// Layout: directories stay small, as a real KB's categories do (results in
// u/gNN/ by 25; a result's own facts in d/gNN/rNNN/sS/ by 50). Writing into
// wide directories made the seed itself quadratic: a commit's index sync runs
// go-git's LastCommitForPath per file, whose tree diff decodes every sibling
// subtree along the path, uncached (knomit #357).

const measureBody = `This fact records an observation made while building the materialize
FactSet measurements. It is written to look like a real fact: a few
paragraphs of prose with caveats, numbers and references, so that the
store's rows, its blob objects and its page cache behave like production.
`

func realisticBody(seed string) string {
	var sb strings.Builder
	for sb.Len() < 2500 {
		sb.WriteString(measureBody)
		sb.WriteString("Seed " + seed + ": the walk reads index rows and edges, never a body.\n\n")
	}
	return sb.String()
}

func factText(t testing.TB, path, title string, refs []string) string {
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

// TestFactSet_MeasureSeed seeds shapes A, B and C (D reuses B) into
// KNOMIT_FACTSET_MEASURE_DB.
func TestFactSet_MeasureSeed(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SEED") == "" {
		t.Skip("set " + measureEnv + "=1 and KNOMIT_FACTSET_MEASURE_SEED=1 to seed")
	}
	f, branch := measureStore(t, true)
	units := envInt("KNOMIT_FACTSET_MEASURE_UNITS", 500)
	// Batches stay near 500 facts: a commit's index sync runs one uncached
	// go-git diff of the whole commit PER FILE (LastCommitForPath, knomit
	// #357), so a batch of n files costs O(n^2); 12,500-fact batches made
	// the seed quadratic.
	const chunk = 1 // results per batch of their own facts (499 of them)
	start := time.Now()
	// KNOMIT_FACTSET_MEASURE_PROFILE_SEED=<from>: seed only B's results
	// from..from+10 (to profile the seed itself on a copy of a store).
	if from := envInt("KNOMIT_FACTSET_MEASURE_PROFILE_SEED", -1); from >= 0 {
		for i := from; i < from+10; i++ {
			files := map[string]string{}
			own := make([]string, 499)
			for j := range own {
				own[j] = fmt.Sprintf("kb/m4/b/d/g%02d/r%03d/s%d/l%03d.md", i/25, i, j/50, j)
				files[own[j]] = factText(t, own[j], fmt.Sprintf("B %d.%d", i, j), nil)
			}
			t1 := time.Now()
			batchWrite(t, f, branch, files)
			t2 := time.Now()
			p := fmt.Sprintf("kb/m4/b/u/g%02d/r%03d.md", i/25, i)
			batchWrite(t, f, branch, map[string]string{p: factText(t, p, fmt.Sprintf("B result %d", i), own)})
			t.Logf("B result %d: leaves %s, result %s", i, t2.Sub(t1), time.Since(t2))
		}
		return
	}

	// A: 499 shared leaves, then the results.
	files := map[string]string{}
	leaves := make([]string, 499)
	for j := range leaves {
		leaves[j] = fmt.Sprintf("kb/m4/a/z/l%03d.md", j)
		files[leaves[j]] = factText(t, leaves[j], fmt.Sprintf("A leaf %d", j), nil)
	}
	batchWrite(t, f, branch, files)
	for lo := 0; lo < units; lo += 100 {
		files = map[string]string{}
		for i := lo; i < min(units, lo+100); i++ {
			p := fmt.Sprintf("kb/m4/a/u/r%03d.md", i)
			files[p] = factText(t, p, fmt.Sprintf("A result %d", i), leaves)
		}
		batchWrite(t, f, branch, files)
	}
	t.Logf("seed A: %s", time.Since(start))

	// B: per result, 499 facts of its own.
	start = time.Now()
	for lo := 0; lo < units; lo += chunk {
		files = map[string]string{}
		results := map[string]string{}
		for i := lo; i < min(units, lo+chunk); i++ {
			own := make([]string, 499)
			for j := range own {
				own[j] = fmt.Sprintf("kb/m4/b/d/g%02d/r%03d/s%d/l%03d.md", i/25, i, j/50, j)
				files[own[j]] = factText(t, own[j], fmt.Sprintf("B %d.%d", i, j), nil)
			}
			p := fmt.Sprintf("kb/m4/b/u/g%02d/r%03d.md", i/25, i)
			results[p] = factText(t, p, fmt.Sprintf("B result %d", i), own)
		}
		batchWrite(t, f, branch, files)
		batchWrite(t, f, branch, results)
		if done := min(units, lo+chunk); done%50 == 0 || done == units {
			t.Logf("seed B: %d results in %s", done, time.Since(start))
		}
	}

	// C: per result, 50 chains of 10 facts of its own, written bottom up.
	start = time.Now()
	const chunkC = 10 // 500 chain facts per batch
	for lo := 0; lo < units; lo += chunkC {
		for k := 9; k >= 0; k-- {
			files = map[string]string{}
			for i := lo; i < min(units, lo+chunkC); i++ {
				for j := range 50 {
					p := fmt.Sprintf("kb/m4/c/d/g%02d/r%03d/s%d/c%02d_%d.md", i/25, i, j/5, j, k)
					var refs []string
					if k < 9 {
						refs = []string{fmt.Sprintf("kb/m4/c/d/g%02d/r%03d/s%d/c%02d_%d.md", i/25, i, j/5, j, k+1)}
					}
					files[p] = factText(t, p, fmt.Sprintf("C %d.%d.%d", i, j, k), refs)
				}
			}
			batchWrite(t, f, branch, files)
		}
		results := map[string]string{}
		for i := lo; i < min(units, lo+chunkC); i++ {
			heads := make([]string, 50)
			for j := range heads {
				heads[j] = fmt.Sprintf("kb/m4/c/d/g%02d/r%03d/s%d/c%02d_0.md", i/25, i, j/5, j)
			}
			p := fmt.Sprintf("kb/m4/c/u/g%02d/r%03d.md", i/25, i)
			results[p] = factText(t, p, fmt.Sprintf("C result %d", i), heads)
		}
		batchWrite(t, f, branch, results)
		if done := min(units, lo+chunkC); done%50 == 0 || done == units {
			t.Logf("seed C: %d results in %s", done, time.Since(start))
		}
	}
}

type measureShape struct {
	prefix   string
	maxNodes int
}

var measureShapes = map[string]measureShape{
	"A": {"kb/m4/a/u/", MaxNodesLimit},
	"B": {"kb/m4/b/u/", MaxNodesLimit},
	"C": {"kb/m4/c/u/", MaxNodesLimit},
	"D": {"kb/m4/b/u/", 200},
}

// TestFactSet_MeasureMatrix measures one shape, twice in this process (the
// first pass cold: fresh process, fresh store open, empty reader caches; the
// second warm), and then 4 concurrent builds of it (the walk cap).
func TestFactSet_MeasureMatrix(t *testing.T) {
	if os.Getenv(measureEnv) == "" || os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE") == "" {
		t.Skip("set " + measureEnv + "=1, KNOMIT_FACTSET_MEASURE_DB and KNOMIT_FACTSET_MEASURE_SHAPE to measure")
	}
	name := os.Getenv("KNOMIT_FACTSET_MEASURE_SHAPE")
	shape, ok := measureShapes[name]
	require.True(t, ok, name)
	f, _ := measureStore(t, false)
	ti := f.svc.ToolSession()
	req := FactSetRequest{Binding: f.b, Search: FactSetSearch{Path: shape.prefix, Sort: SortRelevance},
		Facts: MaxFactSetFacts, Limit: HumanMaxLimit,
		Options: Options{Depth: MaxDepth, MaxNodes: shape.maxNodes}}
	ctx := context.Background()

	for pass, label := range []string{"cold", "warm"} {
		owner := fmt.Sprintf("m4-%s-%d", name, pass)
		runtime.GC()
		stop := peakSampler(ti)
		var prof *os.File
		if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" && pass == 0 {
			var err error
			prof, err = os.Create(filepath.Join(dir, "page1-"+name+".pprof"))
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(prof))
		}
		t1 := time.Now()
		page, st1, err := FactSetFirstPage(ctx, f.scope(owner), req)
		first := time.Since(t1)
		if prof != nil {
			pprof.StopCPUProfile()
			_ = prof.Close()
		}
		require.NoError(t, err)
		var seekProf *os.File
		if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" && pass == 0 {
			var err error
			seekProf, err = os.Create(filepath.Join(dir, "seek-"+name+".pprof"))
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(seekProf))
		}
		t2 := time.Now()
		p, st2, seekErr := FactSetNextPageStats(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, page.Units)
		seek := time.Since(t2)
		if seekProf != nil {
			pprof.StopCPUProfile()
			_ = seekProf.Close()
		}
		seekNote := fmt.Sprintf("walked %d units, %d sections", st2.UnitsWalked, p.Sections)
		if seekErr != nil {
			require.ErrorIs(t, seekErr, ErrFactSetTooLarge)
			seekNote = fmt.Sprintf("422 at the section cap after walking %d more units", st2.UnitsWalked)
		}
		t3 := time.Now()
		var n countWriter
		exportErr := FactSetExport(ctx, f.scope(owner), f.b, nil, nil, *page.Cursor, func(FactSetEnv) io.Writer { return &n })
		export := time.Since(t3)
		exportNote := fmt.Sprintf("%d MiB", n.n>>20)
		if exportErr != nil {
			require.ErrorIs(t, exportErr, ErrFactSetTooLarge)
			exportNote = "422 (past the section cap)"
		}
		heap, _ := stop()
		_, bytes, _ := ti.FactSetFiles()
		w := st2.Walk
		t.Logf("SHAPE %s %s: units %d; page 1 %s (walk %+v); far seek %s (%s; walk %+v); export %s (%s); heap peak %d MiB; factset files %d MiB",
			name, label, page.Units, first, st1.Walk, seek, seekNote, w, export, exportNote, heap>>20, bytes>>20)
	}

	// 4 concurrent builds (the walk cap): first page, far seek, of 4 owners.
	if os.Getenv("KNOMIT_FACTSET_MEASURE_CONCURRENT") == "" {
		return
	}
	runtime.GC()
	stop := peakSampler(ti)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, MaxConcurrentFactSetBuilds)
	for i := range MaxConcurrentFactSetBuilds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("m4-%s-c%d", name, i)
			p, _, err := FactSetFirstPage(ctx, f.scope(owner), req)
			if err == nil {
				_, err = FactSetNextPage(ctx, f.scope(owner), f.b, nil, nil, *p.Cursor, p.Units)
			}
			if errors.Is(err, ErrFactSetTooLarge) {
				err = nil
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	heap, _ := stop()
	for _, err := range errs {
		require.NoError(t, err)
	}
	_, bytes, _ := ti.FactSetFiles()
	t.Logf("SHAPE %s concurrent x%d: wall %s; heap peak %d MiB; factset files %d MiB; factset memory %d KiB",
		name, MaxConcurrentFactSetBuilds, time.Since(start), heap>>20, bytes>>20, sessions.FactSetMemoryUsed()>>10)
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
