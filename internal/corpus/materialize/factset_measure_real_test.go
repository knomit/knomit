package materialize

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/sessions"
)

// TestFactSet_MeasureRealKB reads factsets of a COPY of a real knowledge base
// (KNOMIT_FACTSET_REALDB = the copied .db, KNOMIT_FACTSET_REALBRANCH its agent
// branch) end to end, for searches the results list shows without a text
// query (no embedder here), and reports each factset's units, sections, its
// structure's memory and whether it stayed in memory.
func TestFactSet_MeasureRealKB(t *testing.T) {
	path, branch := os.Getenv("KNOMIT_FACTSET_REALDB"), os.Getenv("KNOMIT_FACTSET_REALBRANCH")
	if path == "" || branch == "" {
		t.Skip("set KNOMIT_FACTSET_REALDB and KNOMIT_FACTSET_REALBRANCH to measure a real knowledge base copy")
	}
	svc, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "real", UID: "uid-real", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	f := &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, branch)}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		s    FactSetSearch
	}{
		{"whole KB, recent (the default list)", FactSetSearch{Sort: SortRecent}},
		{"path kb/architecture/", FactSetSearch{Path: "kb/architecture/", Sort: SortRelevance}},
		{"path kb/decisions/", FactSetSearch{Path: "kb/decisions/", Sort: SortRelevance}},
		{"domain agents", FactSetSearch{Domains: []string{"agents"}, Sort: SortRelevance}},
	} {
		req := FactSetRequest{Binding: f.b, Search: c.s, Facts: DefaultFactSetFacts, Options: DefaultOptions(), Limit: HumanMaxLimit}
		base := sessions.FactSetMemoryUsed()
		runtime.GC()
		stopPeak := peakSampler(svc.ToolSession())
		var prof *os.File
		if dir := os.Getenv("KNOMIT_FACTSET_MEASURE_PROFILE"); dir != "" {
			prof, err = os.Create(filepath.Join(dir, "page1-real-"+strings.ReplaceAll(strings.Fields(c.name)[0]+"-"+strings.Fields(c.name)[1], "/", "_")+".pprof"))
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(prof))
		}
		start := time.Now()
		p, st1, err := FactSetFirstPage(ctx, f.scope(c.name), req)
		require.NoError(t, err, c.name)
		first := time.Since(start)
		if prof != nil {
			pprof.StopCPUProfile()
			_ = prof.Close()
		}
		t.Logf("%s: page 1 walk %+v", c.name, st1.Walk)
		if p.Units == 0 {
			stopPeak()
			t.Logf("%s: no results", c.name)
			continue
		}
		seekStart := time.Now()
		last, err := FactSetNextPage(ctx, f.scope(c.name), f.b, nil, nil, *p.Cursor, p.Units)
		require.NoError(t, err, c.name)
		seek := time.Since(seekStart)
		heap, _ := stopPeak()
		bk, ok := svc.ToolSession().OpenFactSet(*p.Cursor)
		require.True(t, ok)
		spilled := bk.Spilled()
		bk.Close()
		t.Logf("%s: %d units, %d sections; first page %s, far seek to unit %d %s, total %s; heap peak %d MiB; structure %d KiB in memory, spilled %v",
			c.name, p.Units, last.Sections, first, p.Units, seek, time.Since(start), heap>>20, (sessions.FactSetMemoryUsed()-base)>>10, spilled)
	}
}
