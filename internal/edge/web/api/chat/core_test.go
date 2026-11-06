package chat

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/mcp"
	"knomit/internal/engine/llm"
)

// The very first request of an adaptive chat carries the core tools, and the
// opening is not sent to the page as a tools change.
func TestChat_FirstTurnCarriesTheCoreTools(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	m := h.queueAdaptive("o1", 40000, textStep("ok", llm.Usage{Input: 100}))
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	evs := s.until("turn_done")

	req := m.requests()[0]
	for _, n := range mcp.ChatCoreTools {
		require.Greater(t, toolTokens(t, req, n), 0, n)
	}
	for _, e := range evs {
		require.NotEqual(t, "tools", e.Name, "the opening is not a change")
	}
}
