package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// A model that makes the same successful call again and again is answered from
// the first result once, then the turn ends with the no-progress sentence. Any
// provider gets this, Anthropic included, because the chat wires the loop's
// guard for every chat.
//
// SABOTAGE: mount.go, drop `ShortCircuit: mcp.RepeatAnswerable` -> the turn
// runs on to the text and the turn_error assertion fails.
func TestChat_RepeatedSuccessfulCallEndsTheTurn(t *testing.T) {
	h := newHarness(t)
	var steps []step
	for i := 0; i < 6; i++ {
		steps = append(steps, toolStep(fmt.Sprintf("tu_%d", i), "knomit_repos", map[string]any{}, usage))
	}
	steps = append(steps, textStep("never reached", usage))
	h.queue("a1", steps...)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "which repos?"))
	evs := s.until("turn_error")
	var e struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(evs[len(evs)-1].Data, &e))
	require.Equal(t, fmt.Sprintf(llm.NoProgressMessage, "knomit_repos", 3), e.Message)
}
