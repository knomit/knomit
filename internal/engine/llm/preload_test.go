package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeRanker returns a scripted ranking and records the messages it was asked
// about.
type fakeRanker struct {
	out  []ScoredTool
	err  error
	msgs []string
}

func (f *fakeRanker) Rank(_ context.Context, message string) ([]ScoredTool, error) {
	f.msgs = append(f.msgs, message)
	return f.out, f.err
}

// ranked builds a ranking best first: scores descend by one, the last is 1.
func ranked(names ...string) []ScoredTool {
	out := make([]ScoredTool, len(names))
	for i, n := range names {
		out[i] = ScoredTool{Name: n, Score: float64(len(names) - i)}
	}
	return out
}

func newRankedWin(t *testing.T, r ToolRanker, tools ...*ToolDef) (*ToolWindow, Snapshot) {
	t.Helper()
	names := make([]string, len(tools))
	for i, d := range tools {
		names[i] = d.Name
	}
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef("- " + strings.Join(names, "\n- ")), Names: names, Ranker: r})
	require.NoError(t, err)
	_, snap := winSnap(t, tools...)
	return w, snap
}

// newRankedRig is newRig with a ranker on the window.
func newRankedRig(t *testing.T, m *fakeModel, rk ToolRanker, tools ...*ToolDef) *rig {
	t.Helper()
	r := newRig(t, m, tools...)
	names := make([]string, len(tools))
	for i, d := range tools {
		names[i] = d.Name
	}
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef("- " + strings.Join(names, "\n- ")), Names: names, Ranker: rk})
	require.NoError(t, err)
	r.w, r.l.Window = w, w
	return r
}

func TestPreload_StopsAtTheFirstToolThatDoesNotFit(t *testing.T) {
	big, small, filler := winTool("big", 3000), winTool("small", 100), winTool("filler", 100)
	rk := &fakeRanker{out: ranked("big", "small", "filler")}

	w, snap := newRankedWin(t, rk, big, small, filler)
	got := w.Preload(context.Background(), "m", snap, 1000*PreloadDivisor, plenty())
	require.Empty(t, got, "P=1000: big does not fit and small does not jump it")
	require.Empty(t, w.Loaded())

	w, snap = newRankedWin(t, rk, big, small, filler)
	got = w.Preload(context.Background(), "m", snap, 3200*PreloadDivisor, plenty())
	require.Equal(t, []string{"big", "small"}, got, "P=3200: both fit, in rank order; filler is tied for last and is not evidence")
	require.Equal(t, []string{"big", "small"}, w.Loaded())
}

func TestPreload_BudgetIsAnEighthOfUsableRoom(t *testing.T) {
	require.Equal(t, 1536, PreloadBudget(16384-16384/4))
	require.Equal(t, 3072, PreloadBudget(32768-32768/4))
	require.Equal(t, 0, PreloadBudget(0))
	require.Equal(t, 0, PreloadBudget(-5))

	// Through the loop: the same 2000-token tool is refused at 16384 and loaded
	// at 32768, because P derives from window less the reply reserve.
	mid, other := winTool("mid", 2000), winTool("other", 50)
	for _, c := range []struct {
		window int
		loaded bool
	}{{16384, false}, {32768, true}} {
		m := &fakeModel{caps: Caps{ContextWindow: c.window}, steps: []fakeStep{{resp: textResp("ok", Usage{})}}}
		r := newRankedRig(t, m, &fakeRanker{out: ranked("mid", "other")}, mid, other)
		res, err := r.l.RunTurn(context.Background(), "go")
		require.NoError(t, err)
		if c.loaded {
			require.Equal(t, []string{"mid"}, res.Preloaded, "window %d", c.window)
			require.Equal(t, []string{LoadToolsName, "mid"}, toolsOf(m.snaps[0]))
		} else {
			require.Empty(t, res.Preloaded, "window %d", c.window)
			require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]))
		}
	}
}

func TestPreload_SkipsLoadedAndPinsForTheTurn(t *testing.T) {
	old, a, b := winTool("old", 100), winTool("a", 100), winTool("b", 100)
	rk := &fakeRanker{out: ranked("old", "a", "b")}
	w, snap := newRankedWin(t, rk, old, a, b)
	require.False(t, w.Run(loadCall("old"), snap, plenty, nil).IsError)
	w.TakeChanges()
	w.BeginTurn()

	got := w.Preload(context.Background(), "m", snap, 1000*PreloadDivisor, plenty())
	require.Equal(t, []string{"a"}, got, "old is already loaded and costs nothing; b is the lowest score")
	require.Equal(t, []string{"a", "old"}, w.Loaded())

	name, ok := w.EvictOne(nil)
	require.True(t, ok)
	require.Equal(t, "old", name, "pressure takes the tool loaded earlier")
	_, ok = w.EvictOne(nil)
	require.False(t, ok, "the preloaded tool is pinned for the turn")
	require.Equal(t, []string{"a"}, w.Loaded())
}

func TestPreload_NoSignalLoadsNothing(t *testing.T) {
	a, b := winTool("a", 100), winTool("b", 100)
	w, snap := newRankedWin(t, NewLexicalRanker([]ToolDef{*a, *b}), a, b)
	require.Empty(t, w.Preload(context.Background(), "", snap, 8000, plenty()), "an empty message scores every tool alike")
	require.Empty(t, w.Loaded())
	require.Empty(t, w.TakeChanges())
}

func TestPreload_RankerErrorDoesNotFailTheTurn(t *testing.T) {
	a, b := winTool("a", 100), winTool("b", 100)
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{{resp: textResp("fine", Usage{})}}}
	r := newRankedRig(t, m, &fakeRanker{err: errors.New("embedder down")}, a, b)
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, "fine", res.Text)
	require.Equal(t, "embedder down", res.RankerError)
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]), "hook 1 is [load_tools]")
	var ev []*ToolsChange
	for _, e := range r.events {
		if e.Kind == LoopToolsChanged {
			ev = append(ev, e.Tools)
		}
	}
	require.Len(t, ev, 1)
	require.Equal(t, ToolsReasonPreload, ev[0].Reason)
	require.Equal(t, "embedder down", ev[0].RankerError)
	require.Empty(t, ev[0].Loaded)
}

func TestPreload_NeverCausesAnOpeningRefusal(t *testing.T) {
	top, other := winTool("top", 400), winTool("other", 50)
	build := func(extra int) (*rig, *fakeModel) {
		m := &fakeModel{caps: Caps{ContextWindow: 1 << 20}, steps: []fakeStep{{resp: textResp("ok", Usage{})}}}
		r := newRankedRig(t, m, &fakeRanker{out: ranked("top", "other")}, top, other)
		require.NoError(t, r.b.Add(Item{ID: "pad", Hook: HookRepo, Text: strings.Repeat("x", 4*extra)}))
		return r, m
	}
	// Probe the opening size, then pick the window so about 100 tokens are free.
	probe, _ := build(3200)
	base := probe.l.estimate(probe.l.view())
	window := (base + 100) * 4 / 3
	for window-window/4 < base+100 {
		window++
	}

	r, m := build(3200)
	m.caps.ContextWindow = window
	require.GreaterOrEqual(t, PreloadBudget(window-window/4), 400, "setup: P alone would admit the tool")
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err, "the first request is sent, not refused")
	require.Len(t, m.snaps, 1)
	require.Empty(t, res.Preloaded, "the tool does not fit the room left, so nothing is preloaded")
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]))

	// Control: with room, the same ranking preloads the tool.
	r, m = build(3200)
	res, err = r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, []string{"top"}, res.Preloaded)
}

func TestPreload_RunsEveryTurnWithThatTurnsMessage(t *testing.T) {
	a, b := winTool("a", 100), winTool("b", 100)
	rk := &fakeRanker{out: ranked("a", "b")}
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{
		{resp: textResp("one", Usage{Input: 500})},
		{resp: textResp("two", Usage{Input: 600})},
	}}
	r := newRankedRig(t, m, rk, a, b)
	_, err := r.l.RunTurn(context.Background(), "first question")
	require.NoError(t, err)
	_, err = r.l.RunTurn(context.Background(), "second question")
	require.NoError(t, err)
	require.Equal(t, []string{"first question", "second question"}, rk.msgs, "the message of each turn, not the history")
	require.Len(t, m.snaps, 2, "preload adds no model request")
}

func TestPreload_HitSetAtTurnEnd(t *testing.T) {
	a, b, c := winTool("a", 100), winTool("b", 100), winTool("c", 100)
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{
		{resp: toolResp("tu_1", "a", `{}`, Usage{Input: 300})},
		{resp: textResp("done", Usage{Input: 400})},
	}}
	r := newRankedRig(t, m, &fakeRanker{out: ranked("a", "b", "c")}, a, b, c)
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, res.Preloaded)
	require.Equal(t, []string{"a"}, res.PreloadHits, "1 of 2 picks was called")
	require.Equal(t, []string{"a"}, hostNames(r), "the preloaded tool was called without load_tools")
}

func hostNames(r *rig) []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, c.name)
	}
	return out
}

func TestPreload_NoRankerMeansNoPreload(t *testing.T) {
	a, b := winTool("a", 100), winTool("b", 100)
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{{resp: textResp("ok", Usage{})}}}
	r := newRig(t, m, a, b)
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Empty(t, res.Preloaded)
	require.Empty(t, res.RankerError)
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]))
}

func TestPreload_AMissIsRecoveredByLoadTools(t *testing.T) {
	guess, need := winTool("guess", 100), winTool("need", 100)
	other := winTool("other", 100)
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{
		{resp: loadResp("tu_l", Usage{Input: 300}, "need")},
		{resp: toolResp("tu_2", "need", `{}`, Usage{Input: 400})},
		{resp: textResp("done", Usage{Input: 500})},
	}}
	r := newRankedRig(t, m, &fakeRanker{out: ranked("guess", "other", "need")}, guess, need, other)
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, []string{"guess", "other"}, res.Preloaded)
	require.Empty(t, res.PreloadHits, "neither guess was called")
	require.Equal(t, []string{LoadToolsName, "guess", "other"}, toolsOf(m.snaps[0]))
	require.Equal(t, []string{LoadToolsName, "guess", "need", "other"}, toolsOf(m.snaps[1]), "the model loaded the tool the guess missed")
	require.Equal(t, []string{"need"}, hostNames(r))
	require.Equal(t, []string{"need"}, r.decide, "the call still went through the gate")
}

func TestPreload_PreloadedWriteToolStillAsksApproval(t *testing.T) {
	a, b := winTool("a", 100), winTool("b", 100)
	m := &fakeModel{caps: Caps{ContextWindow: 100000}, steps: []fakeStep{
		{resp: toolResp("tu_1", "a", `{}`, Usage{Input: 300})},
		{resp: textResp("done", Usage{Input: 400})},
	}}
	r := newRankedRig(t, m, &fakeRanker{out: ranked("a", "b")}, a, b)
	r.allow = false
	_, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, r.decide, "preloading approves nothing")
	require.Empty(t, r.calls, "a declined call is not run")
}
