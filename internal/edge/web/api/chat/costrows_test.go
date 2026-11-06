package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

type costRowsView struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

func (r costRowsView) sum() float64 { return r.Input + r.CacheRead + r.CacheWrite + r.Output }

// A turn's cost is also split by token kind, each request at its own price tier,
// so the page can show a row's dollars without a price list: the rows add up to
// the turn's cost, in the snapshot and in the live usage events,
// and a request past the tier boundary is priced at the higher tier's rates.
//
// SABOTAGE: chat.go onLoopEvent LoopUsage, drop the `it.CostRows = ` assignment
// or the `"cost_rows"` key -> the matching assertion fails.
func TestChat_CostIsSplitByRowAndAddsUpToTheTurn(t *testing.T) {
	h := newHarness(t)
	low := llm.Usage{Input: 10_000, CacheRead: 20_000, CacheWrite: 30_000, Output: 4_000}
	high := llm.Usage{Input: 100_000, CacheRead: 50_000, CacheWrite: 10_000, Output: 2_000} // prompt 160000: the higher tier
	h.queue("a1", toolStep("tu_r", "knomit_repos", map[string]any{}, low), textStep("one", high))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "first"))
	evs := s.until("turn_done")

	var live []sseEvent
	for _, e := range evs {
		if e.Name == "usage" {
			live = append(live, e)
		}
	}
	require.Len(t, live, 2)
	var first struct {
		Cost *float64      `json:"cost_usd"`
		Rows *costRowsView `json:"cost_rows"`
	}
	require.NoError(t, json.Unmarshal(live[0].Data, &first))
	require.NotNil(t, first.Rows)
	require.InDelta(t, 0.001, first.Rows.Input, 1e-12)
	require.InDelta(t, 0.0002, first.Rows.CacheRead, 1e-12)
	require.InDelta(t, 0.00375, first.Rows.CacheWrite, 1e-12)
	require.InDelta(t, 0.002, first.Rows.Output, 1e-12)
	require.InDelta(t, *first.Cost, first.Rows.sum(), 1e-12)
	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var snap struct {
		Items []struct {
			Kind string        `json:"kind"`
			Cost *float64      `json:"cost_usd"`
			Rows *costRowsView `json:"cost_rows"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &snap))
	turn := snap.Items[0]
	require.Equal(t, "user", turn.Kind)
	require.NotNil(t, turn.Rows)
	require.InDelta(t, 0.051, turn.Rows.Input, 1e-12)
	require.InDelta(t, 0.0027, turn.Rows.CacheRead, 1e-12)
	require.InDelta(t, 0.01, turn.Rows.CacheWrite, 1e-12)
	require.InDelta(t, 0.007, turn.Rows.Output, 1e-12)
	require.InDelta(t, 0.0707, *turn.Cost, 1e-12)
	require.InDelta(t, *turn.Cost, turn.Rows.sum(), 1e-12)
}
