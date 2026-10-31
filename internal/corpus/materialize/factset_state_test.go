package materialize

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	for i := uint64(1); i <= n; i++ {
		h := i * 0x9e3779b97f4a7c15
		_, found := s.get(h)
		require.False(t, found)
		s.put(xentry{h: h, check: i, blob: i * 3})
	}
	require.Equal(t, n, s.len())
	require.Len(t, s.main, 2*extraTail, "two merges happened")
	for i := uint64(1); i <= n; i++ {
		e, ok := s.get(i * 0x9e3779b97f4a7c15)
		require.True(t, ok, "key %d", i)
		require.Equal(t, i, e.check)
		require.Equal(t, i*3, e.blob)
	}
	_, ok := s.get(12345)
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
// identically, across many windows, and a replay after eviction with a
// window that does not start at unit 1 too.
//
// Sabotage: read a window one unit short (hi-1), and the unit after each
// window is planned without its level 0 rows.
func TestFactSet_ReadAheadWindowChangesNothing(t *testing.T) {
	f := newFactSetFixture(t)
	for _, l := range []string{"a", "b", "c", "d"} {
		f.write(t, "kb/z/"+l+".md", strings.ToUpper(l), l+" body.")
	}
	for i := range 14 {
		n := leftPad(i)
		f.write(t, "kb/y/o"+n+".md", "O"+n, "own body.", "kb/z/"+string(rune('a'+i%4))+".md")
		f.write(t, "kb/u/r"+n+".md", "R"+n, "r body.", "kb/y/o"+n+".md", "kb/z/"+string(rune('a'+(i+1)%4))+".md")
	}
	read := func(owner string, window int) []string {
		defer SetFactSetPrefetchWindowForTest(window)()
		DropFactSetCacheForTest()
		req := f.request("kb/u/")
		req.Options.Depth, req.Limit = 3, 2
		return pagesJSON(t, f, owner, f.first(t, owner, req))
	}
	want := read("w1", 1)
	var nodes []FactSetNode
	for _, raw := range want {
		var pg FactSetPage
		require.NoError(t, json.Unmarshal([]byte(raw), &pg))
		nodes = append(nodes, pg.Facts...)
	}
	require.Equal(t, 14, nodes[len(nodes)-1].Unit)
	require.Greater(t, len(nodes), 14*2, "units expanded beyond their roots")
	for _, w := range []int{3, prefetchWindow} {
		require.Equal(t, want, read("w"+leftPad(w), w), "window %d", w)
	}
	defer SetFactSetPrefetchWindowForTest(4)()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 2
	p := f.first(t, "seek", req)
	DropFactSetCacheForTest()
	p, err := FactSetNextPage(context.Background(), f.scope("seek"), f.b, nil, nil, *p.Cursor, 9)
	require.NoError(t, err)
	var nine []FactSetNode
	for _, n := range nodes {
		if n.Unit == 9 {
			nine = append(nine, n)
		}
	}
	require.Equal(t, nine[:len(p.Facts)], p.Facts, "a replay to unit 9 with windows of 4")
}
