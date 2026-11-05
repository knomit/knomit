package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rig is an adaptive loop over fakes. Every host call and every gate decision is
// recorded in order, so a test can say what ran and what asked first.
type rig struct {
	l      *Loop
	b      *Builder
	w      *ToolWindow
	m      *fakeModel
	order  []string // "gate:<tool>" and "host:<tool>" in the order they happened
	calls  []hostCall
	decide []string
	events []LoopEvent
	log    *recLog
	allow  bool
}

func (r *rig) CallTool(_ context.Context, name string, args json.RawMessage) (ToolOutcome, error) {
	r.order = append(r.order, "host:"+name)
	r.calls = append(r.calls, hostCall{name, string(args)})
	if name == "mystery" {
		return ToolOutcome{}, fmt.Errorf("unknown tool %q", name)
	}
	return ToolOutcome{Content: "result of " + name}, nil
}

func (r *rig) Decide(_ context.Context, c ToolUse) (Decision, error) {
	r.order = append(r.order, "gate:"+c.Name)
	r.decide = append(r.decide, c.Name)
	if !r.allow {
		return Decision{Allow: false, Kind: "decline"}, nil
	}
	return Decision{Allow: true, Kind: "allow"}, nil
}

func newRig(t *testing.T, m *fakeModel, tools ...*ToolDef) *rig {
	t.Helper()
	r := &rig{m: m, allow: true, log: &recLog{}}
	r.w, r.b, _ = newWin(t, tools...)
	require.NoError(t, r.b.Remove("u"))
	require.NoError(t, r.b.Add(Item{ID: "sys2", Hook: HookRepo, Text: "instructions"}))
	r.l = &Loop{Model: m, Builder: r.b, Tools: r, Gate: r, Window: r.w, Log: r.log, Sink: func(e LoopEvent) { r.events = append(r.events, e) }}
	return r
}

// The rig's builder is winSnap's: it carries a conversation entry "c1" already.
// loadAhead loads tools through the window as an earlier turn would have.
func (r *rig) loadAhead(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		out := r.w.Run(loadCall(n), r.b.Snapshot(), plenty, nil)
		require.False(t, out.IsError, out.Content)
	}
	r.w.TakeChanges()
	r.w.BeginTurn()
}

func multiResp(u Usage, calls ...ToolUse) *Response {
	var bs []Block
	for i := range calls {
		c := calls[i]
		bs = append(bs, Block{Kind: BlockToolUse, ToolUse: &c})
	}
	return &Response{Blocks: bs, StopReason: StopToolUse, Usage: u}
}

func loadResp(id string, u Usage, names ...string) *Response {
	return multiResp(u, loadCall(names...).withID(id))
}

func (t ToolUse) withID(id string) ToolUse { t.ID = id; return t }

func resultText(s Snapshot, id string) *ToolResult { return resultIn(s, id) }

func toolsOf(s Snapshot) []string { return hook1Names(s) }

func std(t *testing.T, tokens ...int) []*ToolDef {
	t.Helper()
	names := []string{"knomit_query", "knomit_learn", "knomit_update", "knomit_explain"}
	var out []*ToolDef
	for i, tk := range tokens {
		out = append(out, winTool(names[i], tk))
	}
	return out
}

func TestLoop_AdaptiveSendsLoaderAndNoToolSchemas(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{{resp: textResp("hi", Usage{})}}}
	r := newRig(t, m, std(t, 100, 100, 100, 100)...)
	_, err := r.l.RunTurn(context.Background(), "hello")
	require.NoError(t, err)
	require.Len(t, m.snaps, 1)
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]), "six-tool-builder shape: only the loader goes out")
	require.Len(t, r.b.Snapshot().Hooks[HookTools], 4, "the builder still holds every tool")
	require.Empty(t, r.calls)
	require.Empty(t, r.decide)
}

func TestLoop_LoaderCallLoadsAndContinuesInTheSameTurn(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: loadResp("tu_l", Usage{}, "knomit_query")},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100, 100, 100)...)
	res, err := r.l.RunTurn(context.Background(), "find things")
	require.NoError(t, err)
	require.Equal(t, 2, res.Requests)
	require.Equal(t, 1, res.Loads)
	require.Equal(t, 0, res.Evictions)
	require.Equal(t, 1, res.ToolCalls, "the loader call counts as a tool call")
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[0]))
	require.Equal(t, []string{LoadToolsName, "knomit_query"}, toolsOf(m.snaps[1]))
	require.Empty(t, r.calls, "the loader never reaches the host")
	require.Empty(t, r.decide, "the loader never reaches the gate")
	got := resultText(m.snaps[1], "tu_l")
	require.NotNil(t, got, "the loader call is answered, paired")
	require.Equal(t, fmt.Sprintf(LoadedResult, "knomit_query"), got.Content)
	require.False(t, got.IsError)

	// Recorded as an auto decision and as a tool call.
	require.Equal(t, []ApprovalLog{{CallID: "tu_l", Tool: LoadToolsName, Decision: "auto"}}, r.log.ap)
	require.Len(t, r.log.tc, 1)
	require.Equal(t, LoadToolsName, r.log.tc[0].Name)

	var changes []*ToolsChange
	for _, e := range r.events {
		if e.Kind == LoopToolsChanged {
			changes = append(changes, e.Tools)
		}
	}
	require.Len(t, changes, 1)
	require.Equal(t, []string{"knomit_query"}, changes[0].Loaded)
	require.Equal(t, ToolsReasonModel, changes[0].Reason)
}

// Loading a write tool never approves it: the call still goes to the gate first.
// 9b26dd4b: the gate, not the window, decides.
func TestLoop_LoadedWriteToolStillAsksApproval(t *testing.T) {
	steps := func() []fakeStep {
		return []fakeStep{
			{resp: loadResp("tu_l", Usage{}, "knomit_learn")},
			{resp: toolResp("tu_w", "knomit_learn", `{"title":"t"}`, Usage{})},
			{resp: textResp("done", Usage{})},
		}
	}
	t.Run("allowed", func(t *testing.T) {
		m := &fakeModel{steps: steps()}
		r := newRig(t, m, std(t, 100, 100)...)
		_, err := r.l.RunTurn(context.Background(), "write it")
		require.NoError(t, err)
		require.Equal(t, []string{"gate:knomit_learn", "host:knomit_learn"}, r.order, "the gate decides before the host runs, and only for the real tool")
	})
	t.Run("declined", func(t *testing.T) {
		m := &fakeModel{steps: steps()}
		r := newRig(t, m, std(t, 100, 100)...)
		r.allow = false
		_, err := r.l.RunTurn(context.Background(), "write it")
		require.NoError(t, err)
		require.Equal(t, []string{"gate:knomit_learn"}, r.order)
		got := resultText(m.snaps[2], "tu_w")
		require.NotNil(t, got)
		require.Equal(t, DeclinedResult, got.Content)
	})
}

func TestLoop_CallToUnloadedToolIsNotRun(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "knomit_update", `{"blind":true}`, Usage{})},
		{resp: toolResp("tu_2", "knomit_update", `{"blind":false}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100, 100)...)
	res, err := r.l.RunTurn(context.Background(), "update")
	require.NoError(t, err)
	first := resultText(m.snaps[1], "tu_1")
	require.NotNil(t, first)
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_update"), first.Content)
	require.True(t, first.IsError)
	require.Equal(t, []string{LoadToolsName, "knomit_update"}, toolsOf(m.snaps[1]))
	require.Equal(t, 1, res.Loads)
	// The retry, with the schema in view, runs through the gate and the host once.
	require.Equal(t, []string{"gate:knomit_update", "host:knomit_update"}, r.order)
	require.Equal(t, `{"blind":false}`, r.calls[0].args)
	for _, e := range r.events {
		if e.Kind == LoopToolsChanged {
			require.Equal(t, ToolsReasonStaleCall, e.Tools.Reason)
		}
	}
}

// [MASTER CALL, 2026-11-05] The call's arguments were written before the model
// saw the schema, so it is not run even when the loader in the same response
// loaded it.
func TestLoop_SameBatchLoadThenCallIsNotRun(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: multiResp(Usage{},
			loadCall("knomit_query").withID("tu_l"),
			ToolUse{ID: "tu_q", Name: "knomit_query", Args: json.RawMessage(`{"text":"guess"}`)})},
		{resp: toolResp("tu_q2", "knomit_query", `{"text":"real"}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100)...)
	_, err := r.l.RunTurn(context.Background(), "search")
	require.NoError(t, err)

	got := resultText(m.snaps[1], "tu_q")
	require.NotNil(t, got, "the blind call is answered, not dangling")
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_query"), got.Content)
	require.True(t, got.IsError)
	require.Equal(t, fmt.Sprintf(LoadedResult, "knomit_query"), resultText(m.snaps[1], "tu_l").Content)
	require.Equal(t, []string{LoadToolsName, "knomit_query"}, toolsOf(m.snaps[1]))
	// Only the repeat, written after the schema was visible, reached the gate and the host.
	require.Equal(t, []string{"gate:knomit_query", "host:knomit_query"}, r.order)
	require.Equal(t, `{"text":"real"}`, r.calls[0].args)
}

func TestLoop_UnknownToolStillGoesToTheHost(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: toolResp("tu_1", "mystery", `{}`, Usage{})},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, std(t, 100)...)
	res, err := r.l.RunTurn(context.Background(), "x")
	require.NoError(t, err)
	require.Equal(t, []string{"gate:mystery", "host:mystery"}, r.order, "a name outside the catalogue takes the ordinary path")
	require.Equal(t, 0, res.Loads)
	got := resultText(m.snaps[1], "tu_1")
	require.Contains(t, got.Content, "unknown tool")
}

// Over budget by less than the oldest unused tool: that tool alone is evicted
// and the request is sent, not refused.
//
// Sizing (tokens): the first request is the snapshot (~loader 200 + a 50 + b 200
// + c 200 + small). The window is 1468 so the reserve is 367 and the usable
// room 1101. Response 1 reports 1150 prompt tokens, so request 2 is estimated
// at 1150 + the small result entry (~60): over by about 110, less than b's 200.
func TestLoop_EvictsBeforeRefusing(t *testing.T) {
	a, b, c := winTool("knomit_query", 50), winTool("knomit_learn", 200), winTool("knomit_update", 200)
	m := &fakeModel{
		caps: Caps{ContextWindow: 1468},
		steps: []fakeStep{
			{resp: toolResp("tu_1", "knomit_query", `{}`, Usage{Input: 1150})},
			{resp: textResp("done", Usage{Input: 1100})},
		},
	}
	r := newRig(t, m, a, b, c)
	r.loadAhead(t, "knomit_query", "knomit_learn", "knomit_update")
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Len(t, m.snaps, 2, "the request was sent, not refused")
	require.Equal(t, []string{LoadToolsName, "knomit_learn", "knomit_query", "knomit_update"}, toolsOf(m.snaps[0]), "all three fit at the start")
	require.Equal(t, []string{LoadToolsName, "knomit_query", "knomit_update"}, toolsOf(m.snaps[1]), "knomit_learn: the oldest tool not called in this turn")
	require.Equal(t, 1, res.Evictions)
	var pressure []*ToolsChange
	for _, e := range r.events {
		if e.Kind == LoopToolsChanged {
			pressure = append(pressure, e.Tools)
		}
	}
	require.Len(t, pressure, 1)
	require.Equal(t, ToolsReasonPressure, pressure[0].Reason)
	require.Equal(t, []string{"knomit_learn"}, pressure[0].Evicted)
}

// Control for the test above: with a window that fits request 2, nothing is
// evicted, so the eviction there is caused by the budget and nothing else.
func TestLoop_NoEvictionWhenTheRequestFits(t *testing.T) {
	a, b, c := winTool("knomit_query", 50), winTool("knomit_learn", 200), winTool("knomit_update", 200)
	m := &fakeModel{
		caps: Caps{ContextWindow: 4000},
		steps: []fakeStep{
			{resp: toolResp("tu_1", "knomit_query", `{}`, Usage{Input: 1000})},
			{resp: textResp("done", Usage{Input: 1100})},
		},
	}
	r := newRig(t, m, a, b, c)
	r.loadAhead(t, "knomit_query", "knomit_learn", "knomit_update")
	res, err := r.l.RunTurn(context.Background(), "go")
	require.NoError(t, err)
	require.Equal(t, 0, res.Evictions)
	require.Equal(t, toolsOf(m.snaps[0]), toolsOf(m.snaps[1]))
}

// Both loaded tools were called in this turn, so neither may be evicted; the
// unused one goes, and the request is still too big: refused, never sent.
//
// Sizing: window 1214 (reserve 303, usable 911). Request 2 is about 1060
// tokens; evicting c (100) leaves about 960, still over; a and b are pinned.
// Without the pin, evicting a as well (about 860) would let the request out.
func TestLoop_NeverEvictsAToolCalledInTheTurn(t *testing.T) {
	a, b, c := winTool("knomit_query", 100), winTool("knomit_learn", 100), winTool("knomit_update", 100)
	m := &fakeModel{
		caps: Caps{ContextWindow: 1214},
		steps: []fakeStep{
			{resp: multiResp(Usage{Input: 1000},
				ToolUse{ID: "tu_a", Name: "knomit_query", Args: json.RawMessage(`{}`)},
				ToolUse{ID: "tu_b", Name: "knomit_learn", Args: json.RawMessage(`{}`)})},
			{resp: textResp("never", Usage{})},
		},
	}
	r := newRig(t, m, a, b, c)
	r.loadAhead(t, "knomit_query", "knomit_learn", "knomit_update") // a is the oldest load
	res, err := r.l.RunTurn(context.Background(), "go")
	require.ErrorIs(t, err, ErrContextFull)
	require.Len(t, m.snaps, 1, "request 2 was refused before it was sent")
	require.Equal(t, []string{"knomit_learn", "knomit_query"}, r.w.Loaded(), "the tools called this turn stay; the unused one was evicted")
	require.Equal(t, 1, res.Evictions)
	// Both calls were answered and recorded, so the turn is paired.
	h := history(r.b)
	require.Len(t, h, 4)
	require.Len(t, h[3].Blocks, 2)
}

func TestLoop_RefusesWhenNothingEvictable(t *testing.T) {
	a := winTool("knomit_query", 100)
	m := &fakeModel{
		caps: Caps{ContextWindow: 1214},
		steps: []fakeStep{
			{resp: toolResp("tu_a", "knomit_query", `{}`, Usage{Input: 2000})},
			{resp: textResp("never", Usage{})},
		},
	}
	r := newRig(t, m, a)
	r.loadAhead(t, "knomit_query")
	res, err := r.l.RunTurn(context.Background(), "go")
	require.ErrorIs(t, err, ErrContextFull)
	require.Contains(t, err.Error(), "no longer fits the model's context window")
	require.Len(t, m.snaps, 1)
	require.Equal(t, 0, res.Evictions)
	require.Equal(t, []string{"knomit_query"}, r.w.Loaded())
}

func TestLoop_PairingHoldsAcrossEvictionAndReload(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: loadResp("tu_l", Usage{}, "knomit_query")},
		{resp: toolResp("tu_q", "knomit_query", `{}`, Usage{})},
		{resp: textResp("one", Usage{})},
		// turn 2: knomit_query was evicted in between
		{resp: toolResp("tu_stale", "knomit_query", `{}`, Usage{})},
		{resp: toolResp("tu_q2", "knomit_query", `{}`, Usage{})},
		{resp: textResp("two", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100)...)
	_, err := r.l.RunTurn(context.Background(), "turn one")
	require.NoError(t, err)
	r.w.BeginTurn()
	name, ok := r.w.EvictOne(nil)
	require.True(t, ok)
	require.Equal(t, "knomit_query", name)
	_, err = r.l.RunTurn(context.Background(), "turn two")
	require.NoError(t, err)

	require.Len(t, m.snaps, 6)
	require.Equal(t, []string{LoadToolsName}, toolsOf(m.snaps[3]), "evicted: the loader alone at the start of turn two")
	require.Equal(t, []string{LoadToolsName, "knomit_query"}, toolsOf(m.snaps[4]), "reloaded by the stale call")
	for i, s := range m.snaps {
		_, err := renderOllamaChecked(s, "m", OllamaOptions{}, false)
		require.NoError(t, err, "request %d", i)
		for _, it := range conversationItems(s) {
			for _, bl := range it.Entry.Blocks {
				if bl.Kind == BlockToolResult {
					require.True(t, hasToolUse(s, bl.ToolResult.ToolUseID), "request %d: result %s has no call before it", i, bl.ToolResult.ToolUseID)
				}
			}
		}
	}
	require.Len(t, r.calls, 2, "only the two calls made with the schema loaded ran")
}

func hasToolUse(s Snapshot, id string) bool {
	for _, it := range conversationItems(s) {
		for _, bl := range it.Entry.Blocks {
			if bl.Kind == BlockToolUse && bl.ToolUse.ID == id {
				return true
			}
		}
	}
	return false
}

func TestLoop_LoadsCountTowardMaxCalls(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: loadResp("l1", Usage{}, "knomit_query")},
		{resp: loadResp("l2", Usage{}, "knomit_learn")},
		{resp: loadResp("l3", Usage{}, "knomit_update")},
		{resp: loadResp("l4", Usage{}, "knomit_explain")},
		{resp: textResp("never", Usage{})},
	}}
	r := newRig(t, m, std(t, 10, 10, 10, 10)...)
	r.l.MaxCalls = 3
	res, err := r.l.RunTurn(context.Background(), "load everything")
	require.ErrorIs(t, err, ErrTooManyCalls)
	require.Equal(t, 3, res.ToolCalls)
	require.Equal(t, 3, res.Loads)
	require.Len(t, m.snaps, 4)
	last := resultText(r.b.Snapshot(), "l4")
	require.NotNil(t, last, "the fourth loader call is answered, not left dangling")
	require.True(t, last.IsError)
	require.NotContains(t, r.w.Loaded(), "knomit_explain")
}

// SABOTAGE: runCalls, drop the loader's guard.observe -> the first case fails.
func TestRepeatGuard_FailingLoaderCalls(t *testing.T) {
	nope := func(i int) *Response { return loadResp(fmt.Sprintf("n%d", i), Usage{}, "nope") }
	t.Run("three identical failures end the turn", func(t *testing.T) {
		m := &fakeModel{steps: []fakeStep{{resp: nope(1)}, {resp: nope(2)}, {resp: nope(3)}, {resp: nope(4)}, {resp: textResp("never", Usage{})}}}
		r := newRig(t, m, std(t, 10)...)
		res, err := r.l.RunTurn(context.Background(), "x")
		require.ErrorIs(t, err, ErrRepeatedFailure)
		require.Equal(t, RepeatFailureLimit, res.ToolCalls)
		require.Len(t, m.snaps, RepeatFailureLimit)
		require.Contains(t, err.Error(), LoadToolsName)
	})
	t.Run("a success between resets the count", func(t *testing.T) {
		m := &fakeModel{steps: []fakeStep{
			{resp: nope(1)}, {resp: nope(2)},
			{resp: loadResp("ok", Usage{}, "knomit_query")},
			{resp: nope(4)}, {resp: nope(5)},
			{resp: textResp("done", Usage{})},
		}}
		r := newRig(t, m, std(t, 10)...)
		res, err := r.l.RunTurn(context.Background(), "x")
		require.NoError(t, err, "four identical failures in all, but never three in a row")
		require.Equal(t, 5, res.ToolCalls)
	})
}

// After a response that reported 910 tokens (900 prompt + 10 output), evicting a
// tool of 100 tokens and adding a 40-token entry leaves the estimate at
// 910 + (40 - 100): the delta is signed. A static loop keeps its old
// expression.
func TestLoop_EstimateAfterEvictionDoesNotRefuseWrongly(t *testing.T) {
	entry := func() Entry {
		for n := 0; ; n++ {
			e := Entry{ID: "new", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: strings.Repeat("y", n)}}}
			if itemBytes(Item{Entry: &e}) == 160 {
				return e
			}
			if n > 400 {
				t.Fatal("could not build a 160-byte entry")
			}
		}
	}()
	require.Equal(t, 160, itemBytes(Item{Entry: &entry}))
	big := winTool("knomit_query", 100)
	require.Equal(t, 400, itemBytes(Item{Tool: big}))

	setup := func(t *testing.T, adaptive bool) *rig {
		m := &fakeModel{}
		r := newRig(t, m, big, winTool("knomit_learn", 20))
		if !adaptive {
			r.l.Window = nil
		}
		r.loadAhead(t, "knomit_query")
		r.l.haveUsage, r.l.lastTokens = true, 910
		r.l.lastCount = r.l.entryCount()
		r.l.lastBytes = viewBytes(r.l.view())
		return r
	}

	r := setup(t, true)
	before := r.l.estimate(r.l.view())
	require.Equal(t, 910, before, "nothing changed since the response: the reported count")
	r.w.BeginTurn()
	_, ok := r.w.EvictOne(nil)
	require.True(t, ok)
	require.NoError(t, r.b.Add(Item{ID: "new", Hook: HookTurn, Entry: &entry}))
	require.Equal(t, 910+(40-100), r.l.estimate(r.l.view()), "signed delta: 40 tokens added, 100 evicted")

	// Control: the same additions in a loop with no window keep the old
	// expression (the entry's tokens, nothing subtracted).
	s := setup(t, false)
	require.NoError(t, s.b.Add(Item{ID: "new", Hook: HookTurn, Entry: &entry}))
	require.Equal(t, 910+40, s.l.estimate(s.l.view()))

	// And an adaptive loop where only entries were added equals the static one.
	r2 := setup(t, true)
	require.NoError(t, r2.b.Add(Item{ID: "new", Hook: HookTurn, Entry: &entry}))
	require.Equal(t, 910+40, r2.l.estimate(r2.l.view()))
}

func TestLoop_StaticProviderIsUntouched(t *testing.T) {
	m := &fakeModel{caps: Caps{StablePrefix: true}, steps: []fakeStep{{resp: textResp("hi", Usage{})}}}
	l, b := newLoop(t, m, &fakeHost{})
	require.NoError(t, b.Add(Item{ID: "tool.knomit_query", Hook: HookTools, Tool: winTool("knomit_query", 50)}))
	want := b.Snapshot()
	require.Nil(t, l.Window)
	_, err := l.RunTurn(context.Background(), "hi")
	require.NoError(t, err)
	require.Equal(t, want.Hooks[HookTools], m.snaps[0].Hooks[HookTools], "every tool, in the builder's own order, no loader")
	require.Equal(t, want.Hooks[HookSystem], m.snaps[0].Hooks[HookSystem])
}

// da80d551: a turn the model never answered is dropped; the loaded set belongs
// to the window, not to the history, so it survives the drop.
func TestLoop_DroppedTurnKeepsLoadedSet(t *testing.T) {
	ids := []string{"e_user", "e_dup", "e_dup"}
	n := 0
	m := &fakeModel{steps: []fakeStep{
		{resp: loadResp("tu_l", Usage{}, "knomit_query")},
		{resp: textResp("later", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100)...)
	r.l.NewID = func() string { id := ids[n%len(ids)]; n++; return id }
	_, err := r.l.RunTurn(context.Background(), "load it")
	require.ErrorIs(t, err, ErrDuplicateItem)
	require.Len(t, history(r.b), 1, "only the seeded conversation entry: the turn was dropped")
	require.Equal(t, []string{"knomit_query"}, r.w.Loaded(), "the load happened and stays")

	r.l.NewID = nil
	_, err = r.l.RunTurn(context.Background(), "again")
	require.NoError(t, err)
	require.Equal(t, []string{LoadToolsName, "knomit_query"}, toolsOf(m.snaps[1]))
}

// 79f352d3: a dropped turn restores the whole baseline, including lastBytes.
// Turn 1 answers (reports 510 tokens). Turn 2's loader response sets
// lastCount/lastBytes to a larger view, then the turn is dropped. The next
// estimate must start from turn 1's bytes: 510 plus the loaded tool's 400
// bytes (100 tokens) at bytes/4 = 610. With lastBytes left at the dropped
// turn's value the delta loses the assistant entry's bytes and falls short.
func TestLoop_DroppedTurnRestoresLastBytes(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: textResp("ok", Usage{Input: 500, Output: 10})},
		{resp: loadResp("tu_l", Usage{Input: 700, Output: 20}, "knomit_query")},
	}}
	tool := winTool("knomit_query", 100)
	r := newRig(t, m, tool, winTool("knomit_learn", 100))
	_, err := r.l.RunTurn(context.Background(), "first")
	require.NoError(t, err)
	require.Equal(t, int64(510), r.l.lastTokens)
	afterOne := r.l.lastBytes

	ids := []string{"e_user", "e_dup", "e_dup"}
	n := 0
	r.l.NewID = func() string { id := ids[n%len(ids)]; n++; return id }
	_, err = r.l.RunTurn(context.Background(), "load it")
	require.ErrorIs(t, err, ErrDuplicateItem)
	require.Equal(t, []string{"knomit_query"}, r.w.Loaded())
	require.Equal(t, int64(510), r.l.lastTokens, "the dropped response's tokens are not the baseline")
	require.Equal(t, afterOne, r.l.lastBytes, "bytes are restored with the tokens")

	require.Equal(t, 400, itemBytes(Item{Tool: tool}))
	require.Equal(t, 510+400/4, r.l.estimate(r.l.view()))
}
