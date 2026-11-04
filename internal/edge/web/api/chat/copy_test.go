package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// The loop emits a structured warning once; the chat words it with the named
// constant. The usage values are pairwise distinct so the prompt size is the
// sum of three different axes (input, cache read, cache write).
func TestChat_PriceWarningUsesTheNamedCopyOnce(t *testing.T) {
	h := newHarness(t)
	big := llm.Usage{Input: 60000, CacheRead: 30001, CacheWrite: 7003, Output: 5} // prompt 97004
	h.queue("claude-test-x", textStep("one", big), textStep("two", big))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)

	var warnings []map[string]any
	for _, text := range []string{"first", "second"} {
		require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, text))
		evs, _ := drive(t, h, "uid:501", id, s, nil)
		for _, e := range evs {
			if e.Name == "warning" {
				var w map[string]any
				require.NoError(t, json.Unmarshal(e.Data, &w))
				warnings = append(warnings, w)
			}
		}
	}
	require.Len(t, warnings, 1, "one warning per chat, not one per request")
	require.Equal(t, "This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.", warnings[0]["message"])
	require.EqualValues(t, 95000, warnings[0]["threshold"])
	require.EqualValues(t, 97004, warnings[0]["prompt_tokens"])
}

// The number is the card's tier boundary, not the prompt size or the model id:
// each boundary below must appear with its own thousands separators.
func TestPriceWarningText_ShowsTheTierBoundaryFromTheCard(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    llm.PriceWarning
		want string
	}{
		{"first tier", llm.PriceWarning{Threshold: 95000, PromptTokens: 97004, TierTokens: 100000, Model: "m-x"},
			"This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save."},
		{"second tier", llm.PriceWarning{Threshold: 190000, PromptTokens: 192000, TierTokens: 200000, Model: "m-x"},
			"This chat is nearing 200,000 tokens. Past that, each message costs more. Start a new chat to save."},
		{"single tier falls back to threshold", llm.PriceWarning{Threshold: 41, PromptTokens: 43, TierTokens: 0, Model: "m-x"},
			"This chat is nearing 41 tokens. Past that, each message costs more. Start a new chat to save."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, priceWarningText(tc.w))
		})
	}
}

func TestChat_ReasoningOnlyTruncationUsesTheNamedMessage(t *testing.T) {
	cut := func(events ...llm.Event) step {
		return step{
			events: events,
			err: &llm.StopError{Reason: llm.StopMaxTokens, Response: &llm.Response{
				StopReason: llm.StopMaxTokens, Usage: llm.Usage{Input: 5, Output: 7},
			}},
		}
	}
	for _, tc := range []struct {
		name   string
		events []llm.Event
		want   string
	}{
		{"reasoning only", []llm.Event{{Kind: llm.EventReasoning}}, reasoningTruncatedMessage},
		{"text then cut", []llm.Event{{Kind: llm.EventReasoning}, {Kind: llm.EventTextDelta, Text: "partial"}}, "The reply hit the output limit."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.queue("a1", cut(tc.events...))
			id := h.create("uid:501", "a1")
			s := h.attach("uid:501", id)
			require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "think"))
			evs, _ := drive(t, h, "uid:501", id, s, nil)
			last := evs[len(evs)-1]
			require.Equal(t, "turn_error", last.Name)
			var e struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(last.Data, &e))
			require.Equal(t, tc.want, e.Message)
		})
	}
}
