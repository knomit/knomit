package chat

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// The opening counts the core in full and the stub of every other tool. A
// window whose usable room U holds the opening but not the opening plus P (U/8,
// the room the first guess and the message need) is refused at open with the
// approved sentence, not on its first turn (review N1); one with that room
// opens.
//
// SABOTAGE: adaptive.go openWindow, drop the openingTokens check -> the
// refused case opens (201) and the first assertion fails.
func TestMount_OpeningThatLeavesNoRoomRefusesToOpenAndOneWithRoomOpens(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	const reserve = 1000 // the scripted model's MaxOutput
	h.queueAdaptive("o1", 100000, textStep("hi", usage))
	probe := h.createAdaptive("uid:501")
	pc := h.mgr.find(probe, writer.String())
	open := openingTokens(pc.loop.Window, pc.loop.Builder)
	require.Greater(t, open, 2000, "fixture: the opening holds the core and the stubs")

	// U = open + 1: the opening fits U, with one token to spare, and P is U/8.
	tight := open + 1 + reserve
	h.queueAdaptive("o1", tight, textStep("hi", usage))
	code, body := h.createOn("uid:501", "ollama", "o1")
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	p := llm.PreloadBudget(open + 1)
	want := fmt.Sprintf(llm.OpeningTooBigAdaptiveMessage, open+p, "o1", tight, reserve, open+1, llm.OllamaRaiseNumCtxHint)
	require.Contains(t, string(body), want)

	// With room for the opening and P it opens.
	roomy := open*8/7 + 16 + reserve
	h.queueAdaptive("o1", roomy, textStep("hi", usage))
	code, body = h.createOn("uid:501", "ollama", "o1")
	require.Equal(t, http.StatusCreated, code, string(body))
}
