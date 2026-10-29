package materialize

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/sessions"
)

// TestBook_MeasureRealKB reads books of a COPY of a real knowledge base
// (KNOMIT_BOOK_REALDB = the copied .db, KNOMIT_BOOK_REALBRANCH its agent
// branch) end to end, for searches the results list shows without a text
// query (no embedder here), and reports each book's units, sections, its
// structure's memory and whether it stayed in memory.
func TestBook_MeasureRealKB(t *testing.T) {
	path, branch := os.Getenv("KNOMIT_BOOK_REALDB"), os.Getenv("KNOMIT_BOOK_REALBRANCH")
	if path == "" || branch == "" {
		t.Skip("set KNOMIT_BOOK_REALDB and KNOMIT_BOOK_REALBRANCH to measure a real knowledge base copy")
	}
	svc, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.OpenRepo())
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "real", UID: "uid-real", AgentBranch: branch, Svc: svc, OntologyRoot: "kb",
	})
	f := &bookFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, branch)}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		s    BookSearch
	}{
		{"whole KB, recent (the default list)", BookSearch{Sort: SortRecent}},
		{"path kb/architecture/", BookSearch{Path: "kb/architecture/", Sort: SortRelevance}},
		{"path kb/decisions/", BookSearch{Path: "kb/decisions/", Sort: SortRelevance}},
		{"domain agents", BookSearch{Domains: []string{"agents"}, Sort: SortRelevance}},
	} {
		req := BookRequest{Binding: f.b, Search: c.s, Facts: DefaultBookFacts, Options: DefaultOptions(), Limit: HumanMaxLimit}
		base := sessions.BookMemoryUsed()
		start := time.Now()
		p, _, err := BookFirstPage(ctx, f.scope(c.name), req)
		require.NoError(t, err, c.name)
		if p.Units == 0 {
			t.Logf("%s: no results", c.name)
			continue
		}
		last, err := BookNextPage(ctx, f.scope(c.name), f.b, nil, nil, *p.Cursor, p.Units)
		require.NoError(t, err, c.name)
		bk, ok := svc.ToolSession().OpenBook(*p.Cursor)
		require.True(t, ok)
		spilled := bk.Spilled()
		bk.Close()
		t.Logf("%s: %d units, %d sections, walked in %s, structure %d KiB in memory, spilled %v",
			c.name, p.Units, last.Sections, time.Since(start), (sessions.BookMemoryUsed()-base)>>10, spilled)
	}
}
