package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newCoreWin(t *testing.T, core []string, rk ToolRanker, tools ...*ToolDef) (*ToolWindow, Snapshot) {
	t.Helper()
	names := make([]string, len(tools))
	for i, d := range tools {
		names[i] = d.Name
	}
	w, err := NewToolWindow(ToolWindowSpec{
		Loader: LoaderDef("- " + strings.Join(names, "\n- ")), Names: names, Core: core, Ranker: rk,
	})
	require.NoError(t, err)
	_, snap := winSnap(t, tools...)
	return w, snap
}

// The core tools are in the window from the opening, before any call, and a
// call to one is run by the host (the window does not handle it).
//
// SABOTAGE: toolwindow.go NewToolWindow, drop the loop that loads Core -> fails.
func TestCore_IsLoadedFromTheOpening(t *testing.T) {
	w, snap := newCoreWin(t, []string{"core_a", "core_b"}, nil, winTool("core_a", 10), winTool("core_b", 10), winTool("other", 10))
	require.Equal(t, []string{"core_a", "core_b"}, w.Loaded())
	require.Equal(t, []string{LoadToolsName, "core_a", "core_b"}, hook1Names(w.Project(snap)))
	require.False(t, w.Handles("core_a"), "a core tool is loaded, so its calls go to the host")
	require.True(t, w.Handles("other"))
	require.Equal(t, []string{"core_a", "core_b"}, w.Core())
}

// A core tool is never evicted: not by EvictOne, not to make room for a load.
//
// SABOTAGE: toolwindow.go evictOrder, drop the `w.core[n]` test -> fails.
func TestCore_IsNeverEvicted(t *testing.T) {
	w, snap := newCoreWin(t, []string{"core_a"}, nil, winTool("core_a", 10), winTool("b", 10), winTool("c", 10))
	w.Run(loadCall("b"), snap, plenty, nil)
	w.BeginTurn()
	name, ok := w.EvictOne(nil)
	require.True(t, ok)
	require.Equal(t, "b", name)
	_, ok = w.EvictOne(nil)
	require.False(t, ok, "only the core tool and the loader remain")
	require.Equal(t, []string{"core_a"}, w.Loaded())

	// Room for one more tool only: loading c may not evict the core tool.
	out := w.Run(loadCall("c"), snap, func() int { return 5 }, nil)
	require.True(t, out.IsError, out.Content)
	require.Contains(t, out.Content, "no room")
	require.Equal(t, []string{"core_a"}, w.Loaded())
}

// Core tools are not picks: the ranker may rank them first, they are skipped,
// and Picks and the preload change name only what the preload added.
//
// SABOTAGE: toolwindow.go Preload, load without the `w.loaded[name]` skip
// -> fails.
func TestCore_IsNotAPreloadPick(t *testing.T) {
	rk := &fakeRanker{out: ranked("core_a", "x", "y", "z")}
	w, snap := newCoreWin(t, []string{"core_a"}, rk, winTool("core_a", 10), winTool("x", 10), winTool("y", 10), winTool("z", 10))
	got := w.Preload(context.Background(), "m", snap, 1000*PreloadDivisor, plenty())
	require.Equal(t, []string{"x", "y"}, got, "z is tied for last and carries no evidence")
	require.Equal(t, []string{"x", "y"}, w.Picks())
	require.Equal(t, []string{"core_a", "x", "y"}, w.Loaded())
	ch := w.TakeChanges()
	require.Len(t, ch, 1)
	require.Equal(t, []string{"x", "y"}, ch[0].Loaded)
}

// A core name the principal was not shown (it is not in the catalogue) is not
// loaded: a read-only principal's window cannot hold a tool it does not have.
func TestCore_NameOutsideTheCatalogueIsIgnored(t *testing.T) {
	w, snap := newCoreWin(t, []string{"ghost", "a"}, nil, winTool("a", 10), winTool("b", 10))
	require.Equal(t, []string{"a"}, w.Loaded())
	require.Equal(t, []string{LoadToolsName, "a"}, hook1Names(w.Project(snap)))
}

// Observe reports the window's state before any request has been projected:
// what is loaded and what the loaded set costs.
func TestCore_ObserveReportsTheOpeningState(t *testing.T) {
	a, b := winTool("core_a", 100), winTool("core_b", 50)
	w, snap := newCoreWin(t, []string{"core_a", "core_b"}, nil, a, b, winTool("o", 10))
	names, tokens := w.Observe(snap)
	require.Equal(t, []string{"core_a", "core_b"}, names)
	require.InDelta(t, 150, tokens, 2)
}
