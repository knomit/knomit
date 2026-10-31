package materialize

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/storetest"
)

// The extra written set stays a set across its tail merges: 10,000 inserts
// (two merges and a tail) are all found with their values, and absent keys
// are not.
//
// Sabotage: drop the main slice's leftovers in merge, and keys inserted
// before the last merge are lost.
func TestFactSetState_ExtraSetSurvivesMerges(t *testing.T) {
	var s hset
	const n = 2*extraTail + 1808
	// Descending keys: every merge's new keys sort before the main slice's,
	// so a merge that dropped the main slice's leftovers would lose them all.
	key := func(i uint64) uint64 { return (n + 1 - i) * 1000003 }
	for i := uint64(1); i <= n; i++ {
		h := key(i)
		_, found := s.get(h)
		require.False(t, found)
		s.put(xentry{h: h, check: i, blob: i * 3})
	}
	require.Equal(t, n, s.len())
	require.Len(t, s.main, 2*extraTail, "two merges happened")
	for i := uint64(1); i <= n; i++ {
		e, ok := s.get(key(i))
		require.True(t, ok, "key %d", i)
		require.Equal(t, i, e.check)
		require.Equal(t, i*3, e.blob)
	}
	_, ok := s.get(12345)
	for i := 1; i < len(s.main); i++ {
		require.Less(t, s.main[i-1].h, s.main[i].h, "the main slice stays sorted")
	}
	require.False(t, ok)
}

// freeze keeps the FIRST candidate of each hash (candidates come in unit
// order: the earliest owner wins), sorts by hash, and refuses two
// candidates of one hash with different checks.
//
// Sabotage: keep the last candidate of a hash, and unit 3 owns h=7.
func TestFactSetState_FreezeKeepsTheEarliestOwner(t *testing.T) {
	cands := []fentry{{h: 9, check: 1, unit: 1, k: 1}, {h: 7, check: 2, unit: 1, k: 2}, {h: 7, check: 2, unit: 3, k: 1}, {h: 3, check: 4, unit: 2, k: 1}}
	tab, err := freeze(cands, func(fentry) (string, string) { return "", "" })
	require.NoError(t, err)
	require.Equal(t, []uint64{3, 7, 9}, []uint64{tab[0].h, tab[1].h, tab[2].h})
	i := tab.find(7)
	require.Equal(t, uint16(1), tab[i].unit)
	require.Equal(t, uint16(2), tab[i].k)
	require.Equal(t, -1, tab.find(8))
	_, err = freeze([]fentry{{h: 5, check: 1}, {h: 5, check: 2}}, func(fentry) (string, string) { return "r", "p" })
	require.ErrorAs(t, err, &errHashCollision{})
}

// The read-ahead window changes no byte: the same FactSet read with windows
// of 1, 3 and the default (with its background pipelining) pages
// identically, across many windows; and every page re-read by its cursor
// after an eviction (a replay, its windows aligned from unit 1) is the page
// first served from the cache (windows aligned at the page's unit).
//
// The fixture holds one target blob pinned at TWO commits (t reverted to its
// first bytes, the two commits dated years apart): roots in one window cite
// it at each commit, so a read keyed by blob alone would serve one commit's
// date for the other (review B2).
//
// Sabotage: key the versions read by (path, blob) and drop the commit, and
// the window changes t's date.
func TestFactSet_ReadAheadWindowChangesNothing(t *testing.T) {
	f := newFactSetFixture(t)
	db := storetest.RawDB(f.svc)
	setDate := func(commit, path string, ts int64) {
		res, err := db.Exec(`UPDATE commit_log SET committed_at = ? WHERE commit_hash = ? AND path = ?`, ts, commit, path)
		require.NoError(t, err)
		n, _ := res.RowsAffected()
		require.Equal(t, int64(1), n)
	}
	c1 := f.write(t, "kb/z/t.md", "T", "t v1.")
	setDate(c1, "kb/z/t.md", 1577880000) // 2020-01-01
	f.write(t, "kb/u/r900.md", "R900", "cites t at c1.", "kb/z/t.md")
	f.write(t, "kb/z/t.md", "T", "t v2.")
	c3 := f.write(t, "kb/z/t.md", "T", "t v1.") // the v1 bytes again: one blob, commit c3
	setDate(c3, "kb/z/t.md", 1686009600)        // 2023-06-06
	f.write(t, "kb/u/r901.md", "R901", "cites t at c3.", "kb/z/t.md")
	for _, l := range []string{"a", "b", "c", "d"} {
		f.write(t, "kb/z/"+l+".md", strings.ToUpper(l), l+" body.")
	}
	for i := range 14 {
		n := leftPad(i)
		f.write(t, "kb/y/o"+n+".md", "O"+n, "own body.", "kb/z/"+string(rune('a'+i%4))+".md")
		f.write(t, "kb/u/r"+n+".md", "R"+n, "r body.", "kb/y/o"+n+".md", "kb/z/"+string(rune('a'+(i+1)%4))+".md")
	}
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 2
	read := func(owner string, window int) []string {
		defer SetFactSetPrefetchWindowForTest(window)()
		DropFactSetCacheForTest()
		return pagesJSON(t, f, owner, f.first(t, owner, req))
	}
	want := read("w1", 1)
	var nodes []FactSetNode
	for _, raw := range want {
		var pg FactSetPage
		require.NoError(t, json.Unmarshal([]byte(raw), &pg))
		nodes = append(nodes, pg.Facts...)
	}
	require.Equal(t, 16, nodes[len(nodes)-1].Unit)
	require.Greater(t, len(nodes), 16*2, "units expanded beyond their roots")
	var tDates []string
	for _, n := range nodes {
		if n.Path == "kb/z/t.md" {
			tDates = append(tDates, n.Commit[:8]+" "+n.Date)
		}
	}
	require.ElementsMatch(t, []string{c1[:8] + " 2020-01-01", c3[:8] + " 2023-06-06"}, tDates,
		"t at each commit with that commit's date")
	for _, w := range []int{3, prefetchWindow} {
		require.Equal(t, want, read("w"+leftPad(w), w), "window %d", w)
	}

	// Cache hit against replay, page by page, at the default window.
	p := f.first(t, "cmp", req)
	id := mustID(t, *p.Cursor)
	for p.HasMore {
		cur := *p.Cursor
		var err error
		p, err = FactSetNextPage(ctx, f.scope("cmp"), f.b, nil, nil, cur, 0)
		require.NoError(t, err)
		hit := strings.ReplaceAll(mustJSON(t, p), id, "ID")
		DropFactSetCacheForTest()
		again, err := FactSetNextPage(ctx, f.scope("cmp"), f.b, nil, nil, cur, 0)
		require.NoError(t, err)
		require.Equal(t, hit, strings.ReplaceAll(mustJSON(t, again), id, "ID"), "cursor %s: replay == cache hit", cur)
	}
}
