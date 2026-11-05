package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
)

// The very first turn of an adaptive chat shows the core tools as loaded, with
// their size, though nothing was changed yet, and the first request carries
// them. A static chat is untouched (TestChat_StaticTurnDoneHasNoTools).
//
// SABOTAGE: adaptive.go openWindow, drop the win.Observe lines -> the page
// would say 0 tools loaded on the first turn and this fails.
func TestChat_FirstTurnShowsTheCoreTools(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	m := h.queueAdaptive("o1", 40000, textStep("ok", llm.Usage{Input: 100}))
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	evs := s.until("turn_done")

	req := m.requests()[0]
	want := 0
	for _, n := range mcp.ChatCoreTools {
		want += toolTokens(t, req, n)
	}
	var d struct {
		Tools *turnTools `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &d))
	require.NotNil(t, d.Tools)
	require.Equal(t, len(mcp.ChatCoreTools), d.Tools.LoadedNow)
	require.Equal(t, want, d.Tools.Tokens)
	require.Equal(t, 0, d.Tools.Loaded, "nothing was loaded this turn")
	for _, e := range evs {
		require.NotEqual(t, "tools", e.Name, "the opening is not a change")
	}
}
