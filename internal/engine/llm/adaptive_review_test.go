package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// N2: a Window can never be used with a StablePrefix model; the loop refuses
// before anything is added or sent.
func TestLoop_RefusesWindowWithStablePrefix(t *testing.T) {
	m := &fakeModel{caps: Caps{StablePrefix: true}, steps: []fakeStep{{resp: textResp("hi", Usage{})}}}
	r := newRig(t, m, std(t, 100)...)
	before := len(history(r.b))
	_, err := r.l.RunTurn(context.Background(), "hi")
	require.ErrorIs(t, err, ErrWindowWithStablePrefix)
	require.Empty(t, m.snaps, "nothing was sent")
	require.Len(t, history(r.b), before, "nothing was added")
}

// N5: the reverse order of the same-batch rule. The call comes first, so the
// stale-call path loads X and does not run it; the loader then finds it loaded.
func TestLoop_SameBatchCallThenLoadIsNotRunAndLoads(t *testing.T) {
	m := &fakeModel{steps: []fakeStep{
		{resp: multiResp(Usage{},
			ToolUse{ID: "tu_q", Name: "knomit_query", Args: json.RawMessage(`{"text":"guess"}`)},
			loadCall("knomit_query").withID("tu_l"))},
		{resp: textResp("done", Usage{})},
	}}
	r := newRig(t, m, std(t, 100, 100)...)
	_, err := r.l.RunTurn(context.Background(), "search")
	require.NoError(t, err)
	require.Empty(t, r.order, "X reached neither the gate nor the host")
	require.Equal(t, fmt.Sprintf(NotLoadedCallResult, "knomit_query"), resultText(m.snaps[1], "tu_q").Content)
	require.NotNil(t, resultText(m.snaps[1], "tu_l"), "the loader call is answered too")
	require.Equal(t, []string{LoadToolsName, "knomit_query"}, toolsOf(m.snaps[1]))
}

// N1: a catalogue name the Builder has no tool for is never "loaded now": it
// cannot be sent, so a call to it gets the not-in-catalogue answer, like the
// loader path.
func TestWindow_StaleCallToNameWithoutBuilderToolIsNotLoaded(t *testing.T) {
	real := winTool("knomit_query", 100)
	w, err := NewToolWindow(ToolWindowSpec{Loader: LoaderDef("- knomit_query\n- ghost"), Names: []string{"knomit_query", "ghost"}})
	require.NoError(t, err)
	_, snap := winSnap(t, real)

	out := w.Run(ToolUse{ID: "t", Name: "ghost", Args: json.RawMessage(`{}`)}, snap, plenty, nil)
	require.True(t, out.IsError)
	require.Equal(t, w.unknownLine("ghost"), out.Content)
	require.Empty(t, w.Loaded())
	require.Empty(t, w.TakeChanges())
}
