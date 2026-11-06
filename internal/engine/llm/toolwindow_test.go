package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// winTool is a catalogue tool whose definition is about `tokens` tokens (bytes/4).
func winTool(name string, tokens int) *ToolDef {
	d := &ToolDef{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}
	base, _ := encodeCanonical(d)
	d.Description = strings.Repeat("x", max(tokens*4-len(base), 0))
	return d
}

func winSnap(t *testing.T, tools ...*ToolDef) (*Builder, Snapshot) {
	t.Helper()
	b := NewBuilder()
	for _, d := range tools {
		require.NoError(t, b.Add(Item{ID: "tool." + d.Name, Hook: HookTools, Tool: d}))
	}
	require.NoError(t, b.Add(Item{ID: "system", Hook: HookSystem, Text: "sys"}))
	require.NoError(t, b.Add(Item{ID: "c1", Hook: HookConversation, Entry: &Entry{ID: "c1", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "hi"}}}}))
	require.NoError(t, b.Add(Item{ID: "u", Hook: HookTurn, Entry: &Entry{ID: "u", Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "now"}}}}))
	return b, b.Snapshot()
}

func newWin(t *testing.T, tools ...*ToolDef) (*ToolWindow, *Builder, Snapshot) {
	t.Helper()
	names := make([]string, len(tools))
	for i, d := range tools {
		names[i] = d.Name
	}
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef("- " + strings.Join(names, "\n- ")), Names: names})
	require.NoError(t, err)
	b, snap := winSnap(t, tools...)
	return w, b, snap
}

func plenty() int { return 1 << 30 }

func loadCall(names ...string) ToolUse {
	raw, _ := json.Marshal(map[string][]string{"names": names})
	return ToolUse{ID: "tu", Name: LoadToolsName, Args: raw}
}

// isStub reports whether a hook-1 tool is a stub (not a full definition).
func isStub(d *ToolDef) bool { return string(d.Schema) == stubSchema }

// hook1Names are the full definitions in hook 1 (the loader and the loaded
// tools); stubs are not in it, see hook1Stubs.
func hook1Names(s Snapshot) []string {
	var out []string
	for _, it := range s.Hooks[HookTools] {
		if !isStub(it.Tool) {
			out = append(out, it.Tool.Name)
		}
	}
	return out
}

// hook1Stubs are the names of the stubs in hook 1.
func hook1Stubs(s Snapshot) []string {
	var out []string
	for _, it := range s.Hooks[HookTools] {
		if isStub(it.Tool) {
			out = append(out, it.Tool.Name)
		}
	}
	return out
}

func TestWindow_NewRejectsACatalogueToolNamedLikeTheLoader(t *testing.T) {
	_, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef(""), Names: []string{"a", LoadToolsName}})
	require.ErrorContains(t, err, LoadToolsName)
	_, err = NewToolWindow(ToolWindowSpec{Loader: ToolDef{Name: "other"}, Names: []string{"a"}})
	require.ErrorContains(t, err, LoadToolsName)
}

func TestWindow_ProjectSendsLoaderThenLoadedByName(t *testing.T) {
	w, _, snap := newWin(t, winTool("b", 10), winTool("a", 10), winTool("c", 10))
	require.Equal(t, []string{LoadToolsName}, hook1Names(w.Project(snap)), "nothing loaded: the loader alone")
	out := w.Run(loadCall("c", "a"), snap, plenty, nil)
	require.False(t, out.IsError)
	p := w.Project(snap)
	require.Equal(t, []string{LoadToolsName, "a", "c"}, hook1Names(p), "loader first, then the loaded tools in snapshot order; b absent")
	for h := HookSystem; h <= HookTurn; h++ {
		require.Equal(t, snap.Hooks[h], p.Hooks[h], "hook %d is untouched", h)
	}
}

func TestWindow_ProjectLeavesTheBuilderUntouched(t *testing.T) {
	w, b, snap := newWin(t, winTool("b", 10), winTool("a", 10), winTool("c", 10))
	w.Run(loadCall("a"), snap, plenty, nil)
	before := b.Snapshot()
	p := w.Project(snap)
	p.Hooks[HookTools][0].ID = "mutated"
	p.Hooks[HookConversation][0].ID = "mutated"
	after := b.Snapshot()
	require.Equal(t, before, after, "the builder still holds every tool and the whole history")
	require.Len(t, after.Hooks[HookTools], 3)
	require.ErrorIs(t, b.Replace("c1", Item{ID: "c1", Hook: HookConversation}), ErrHistoryAppendOnly)
}

func TestWindow_LoadManyInOneCall(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10), winTool("c", 10))
	out := w.Run(loadCall("a", "c"), snap, plenty, nil)
	require.False(t, out.IsError)
	require.Equal(t, fmt.Sprintf(LoadedResult, "a, c"), out.Content)
	require.Equal(t, []string{"a", "c"}, w.Loaded())
}

func TestWindow_LoadUnknownNameListsNearest(t *testing.T) {
	w, _, snap := newWin(t, winTool("knomit_query", 10), winTool("knomit_repos", 10))
	out := w.Run(loadCall("query"), snap, plenty, nil)
	require.True(t, out.IsError, "nothing loaded and nothing changed")
	require.Contains(t, out.Content, fmt.Sprintf(NotInCatalogueResult, "query", "knomit_query"))
	require.NotContains(t, out.Content, "knomit_repos)", "knomit_repos is not a near match")
	require.Contains(t, out.Content, fmt.Sprintf(NothingLoadedResult, "knomit_query, knomit_repos"))
	require.Empty(t, w.Loaded())
}

func TestWindow_LoadMixedKnownAndUnknown(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10))
	out := w.Run(loadCall("a", "zzz"), snap, plenty, nil)
	require.False(t, out.IsError)
	require.Contains(t, out.Content, fmt.Sprintf(LoadedResult, "a"))
	require.Contains(t, out.Content, fmt.Sprintf(NotInCatalogueNoMatchResult, "zzz"))
	require.NotContains(t, out.Content, "Nothing was loaded")
	require.Equal(t, []string{"a"}, w.Loaded())
}

func TestWindow_LoadAlreadyLoadedIsReportedNotRepeated(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10))
	w.Run(loadCall("a"), snap, plenty, nil)
	w.TakeChanges()
	out := w.Run(loadCall("a", "b"), snap, plenty, nil)
	require.False(t, out.IsError)
	require.Contains(t, out.Content, fmt.Sprintf(LoadedResult, "b"))
	require.Contains(t, out.Content, fmt.Sprintf(AlreadyLoadedResult, "a"))
	ch := w.TakeChanges()
	require.Len(t, ch, 1)
	require.Equal(t, []string{"b"}, ch[0].Loaded, "a already loaded is not reported as loaded again")

	only := w.Run(loadCall("a"), snap, plenty, nil)
	require.True(t, only.IsError, "nothing newly loaded")
	require.Equal(t, fmt.Sprintf(AlreadyLoadedResult, "a"), only.Content)
	require.Empty(t, w.TakeChanges())
}

func TestWindow_LoadRefusedWhenNoRoom(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 100), winTool("b", 300))
	free := 150
	out := w.Run(loadCall("a", "b"), snap, func() int { return free }, nil)
	require.False(t, out.IsError, "a was loaded")
	require.Equal(t, []string{"a"}, w.Loaded())
	// Free room is measured with every stub in the request, so a load costs
	// its growth: the full definition less the stub it replaces.
	require.Equal(t, 100-w.stubBytes["a"]/4, w.growth("a"))
	left := 150 - w.growth("a")
	require.Contains(t, out.Content, fmt.Sprintf(NoRoomResult, "b", w.growth("b"), left), "b needs its growth over the stub; what a left of 150 is free")
}

func TestWindow_EvictOneIsLRUAndSkipsPinned(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10), winTool("c", 10))
	w.Run(loadCall("a"), snap, plenty, nil)
	w.Run(loadCall("b"), snap, plenty, nil)
	w.Run(loadCall("c"), snap, plenty, nil)
	w.Touch("a") // a is now the most recently used
	w.BeginTurn()
	name, ok := w.EvictOne(Pinned{"b": true})
	require.True(t, ok)
	require.Equal(t, "c", name, "b is pinned and a is newest: c is the oldest unpinned")
	name, ok = w.EvictOne(Pinned{"b": true})
	require.True(t, ok)
	require.Equal(t, "a", name)
	_, ok = w.EvictOne(Pinned{"b": true})
	require.False(t, ok, "only pinned b and the loader remain")
	require.Equal(t, []string{"b"}, w.Loaded())
}

func TestWindow_EvictOneBreaksRecencyTiesByLargerDefinition(t *testing.T) {
	w, _, snap := newWin(t, winTool("small", 10), winTool("large", 90), winTool("mid", 40))
	w.Run(loadCall("small", "large", "mid"), snap, plenty, nil) // one call: one recency
	w.BeginTurn()
	first, _ := w.EvictOne(nil)
	second, _ := w.EvictOne(nil)
	third, _ := w.EvictOne(nil)
	require.Equal(t, []string{"large", "mid", "small"}, []string{first, second, third})
}

func TestWindow_NeverEvictsTheLoader(t *testing.T) {
	w, _, _ := newWin(t, winTool("a", 10))
	_, ok := w.EvictOne(nil)
	require.False(t, ok)
	require.Empty(t, w.Loaded())
}

func TestWindow_ToolsLoadedThisTurnAreNotEvictable(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10))
	w.Run(loadCall("a"), snap, plenty, nil)
	_, ok := w.EvictOne(nil)
	require.False(t, ok, "a was loaded in this turn")
	w.BeginTurn()
	name, ok := w.EvictOne(nil)
	require.True(t, ok)
	require.Equal(t, "a", name)
}

func TestWindow_LoadMakesRoomByEvictingOnlyWhatIsEnough(t *testing.T) {
	w, _, snap := newWin(t, winTool("old", 100), winTool("mid", 100), winTool("new", 150))
	w.Run(loadCall("old"), snap, plenty, nil)
	w.Run(loadCall("mid"), snap, plenty, nil)
	w.BeginTurn()
	w.TakeChanges()
	// 60 free: "new" (150) needs 90 more, so evicting old (100) is enough.
	out := w.Run(loadCall("new"), snap, func() int { return 60 }, nil)
	require.False(t, out.IsError)
	require.Contains(t, out.Content, fmt.Sprintf(EvictedResult, "old"))
	require.Equal(t, []string{"mid", "new"}, w.Loaded())
	ch := w.TakeChanges()
	require.Len(t, ch, 1)
	require.Equal(t, []string{"new"}, ch[0].Loaded)
	require.Equal(t, []string{"old"}, ch[0].Evicted)
	require.Equal(t, ToolsReasonModel, ch[0].Reason)
	require.Equal(t, []string{"mid", "new"}, ch[0].LoadedNow)
	// What the loaded set adds to a request: each definition less its stub.
	require.Equal(t, (100-w.stubBytes["mid"]/4)+(150-w.stubBytes["new"]/4), ch[0].Tokens)
	require.Less(t, ch[0].Tokens, 100+150)
}

func TestWindow_NoRoomEvenAfterEvictingChangesNothing(t *testing.T) {
	w, _, snap := newWin(t, winTool("old", 100), winTool("huge", 900))
	w.Run(loadCall("old"), snap, plenty, nil)
	w.BeginTurn()
	w.TakeChanges()
	out := w.Run(loadCall("huge"), snap, func() int { return 60 }, nil)
	require.True(t, out.IsError)
	require.Equal(t, []string{"old"}, w.Loaded(), "evicting old would not have made room, so it stays")
	require.Empty(t, w.TakeChanges())
}

func TestWindow_NotLoadedCallIsAnsweredAndLoaded(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10), winTool("b", 10))
	require.True(t, w.Handles("a"), "a catalogue tool that is not loaded is the window's to answer")
	require.False(t, w.Handles("zzz"), "a name outside the catalogue goes to the host")
	out := w.Run(ToolUse{ID: "x", Name: "a", Args: json.RawMessage(`{"q":1}`)}, snap, plenty, nil)
	require.True(t, out.IsError)
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "a"), out.Content)
	require.Equal(t, []string{"a"}, w.Loaded())
	w.BeginBatch()
	require.False(t, w.Handles("a"), "once loaded, in a later batch the call runs")
	require.True(t, w.Handles(LoadToolsName))
	ch := w.TakeChanges()
	require.Len(t, ch, 1)
	require.Equal(t, ToolsReasonStaleCall, ch[0].Reason)
}

func TestWindow_LoadedInThisBatchIsStillNotRun(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10))
	w.BeginBatch()
	w.Run(loadCall("a"), snap, plenty, nil)
	require.True(t, w.Handles("a"), "a was loaded by this batch's loader: a call in the same batch is not run")
	out := w.Run(ToolUse{ID: "x", Name: "a", Args: json.RawMessage(`{}`)}, snap, plenty, nil)
	require.True(t, out.IsError)
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "a"), out.Content)
	w.BeginBatch()
	require.False(t, w.Handles("a"))
}

func TestWindow_BadLoaderArgumentsLoadNothing(t *testing.T) {
	w, _, snap := newWin(t, winTool("a", 10))
	for _, raw := range []string{`{}`, `{"names":[]}`, `{"names":"a"}`, `[]`} {
		out := w.Run(ToolUse{ID: "x", Name: LoadToolsName, Args: json.RawMessage(raw)}, snap, plenty, nil)
		require.True(t, out.IsError, raw)
		require.Equal(t, fmt.Sprintf(NothingLoadedResult, "a"), out.Content, raw)
	}
	require.Empty(t, w.Loaded())
}

func TestLoaderDef_CarriesTheApprovedCopy(t *testing.T) {
	d := LoaderDef("- a: does a")
	require.Equal(t, LoadToolsName, d.Name)
	require.True(t, strings.HasPrefix(d.Description, LoaderDescription))
	require.True(t, strings.HasSuffix(d.Description, "- a: does a"))
	var s struct {
		Properties map[string]struct {
			Description string `json:"description"`
			MinItems    int    `json:"minItems"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(d.Schema, &s))
	require.Equal(t, LoaderNamesParamDescription, s.Properties["names"].Description)
	require.Equal(t, 1, s.Properties["names"].MinItems)
	require.Equal(t, []string{"names"}, s.Required)
}
