package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
)

// toolCallEvents splits a turn's tool_call events into running and done, by call id.
func toolCallEvents(t *testing.T, evs []sseEvent) (running, done map[string]map[string]any) {
	t.Helper()
	running, done = map[string]map[string]any{}, map[string]map[string]any{}
	for _, e := range evs {
		if e.Name != "tool_call" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal(e.Data, &m))
		if m["state"] == "running" {
			running[m["id"].(string)] = m
		} else {
			done[m["id"].(string)] = m
		}
	}
	return running, done
}

// snapshotCalls is GET /chats/{id}'s tool_call rows, by id, as raw maps.
func snapshotCalls(t *testing.T, h *harness, who, id string) map[string]map[string]any {
	t.Helper()
	code, body := h.do(who, http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var v struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &v))
	out := map[string]map[string]any{}
	for _, it := range v.Items {
		if it["kind"] == "tool_call" {
			out[it["id"].(string)] = it
		}
	}
	return out
}

// A call's row wording comes from the tool's own definition: the running and
// done events, the snapshot and the call endpoint all carry the tool's title
// and the value of the argument the tool marks as its main one. A tool the
// chat does not know (here a misspelt name the model made up) has neither.
//
// SABOTAGE: chat.go, leave title off the running event -> fails; leave
// key_arg off the snapshot item -> fails.
func TestChat_ToolCallCarriesTitleAndKeyArg(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000081", "alpha")
	h.queue("a1",
		toolStep("tu_b", "knomit_bind", map[string]any{"repo": "alpha"}, usage),
		toolStep("tu_q", "knomit_query", map[string]any{"binding": handle, "text": "approval gate"}, usage),
		toolStep("tu_x", "knomit_nope", map[string]any{"text": "x"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "search"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	running, done := toolCallEvents(t, evs)

	for _, ev := range []map[string]map[string]any{running, done} {
		require.Equal(t, "Bind", ev["tu_b"]["title"])
		require.Equal(t, "alpha", ev["tu_b"]["key_arg"])
		require.Equal(t, "Query", ev["tu_q"]["title"])
		require.Equal(t, "approval gate", ev["tu_q"]["key_arg"])
		require.Equal(t, "knomit_nope", ev["tu_x"]["name"], "the unknown call has its events")
		require.NotContains(t, ev["tu_x"], "title", "an unknown tool has no title: the page shows its name")
		require.NotContains(t, ev["tu_x"], "key_arg")
	}

	snap := snapshotCalls(t, h, "uid:501", id)
	require.Equal(t, "Bind", snap["tu_b"]["title"])
	require.Equal(t, "alpha", snap["tu_b"]["key_arg"])
	require.Equal(t, "Query", snap["tu_q"]["title"])
	require.Equal(t, "approval gate", snap["tu_q"]["key_arg"])
	require.Equal(t, "knomit_nope", snap["tu_x"]["name"])
	require.NotContains(t, snap["tu_x"], "title")

	rec := callResult(t, h, "uid:501", id, "tu_q")
	require.Equal(t, "Query", rec.Title)
	require.Equal(t, "approval gate", rec.KeyArg)
}

// The loader's row: its title is "Load tools", and its main argument is what
// it LOADED (not what the model asked for), on the done event and the snapshot.
//
// SABOTAGE: chat.go, take the loader's key_arg from its arguments -> it names
// what was asked for, and the asked-for list here differs.
func TestChat_LoaderCallCarriesTitleAndWhatItLoaded(t *testing.T) {
	h := newHarness(t)
	h.useOllama()
	h.queueAdaptive("o1", 40000,
		// knomit_query is a core tool, already loaded: the loader loads only explain.
		loadStep("tu_load", llm.Usage{Input: 50}, "knomit_explain", "knomit_query"),
		textStep("ok", llm.Usage{Input: 100}),
	)
	id := h.createAdaptive("uid:501")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, blankMessage))
	_, done := toolCallEvents(t, s.until("turn_done"))
	require.Equal(t, "Load tools", done["tu_load"]["title"])
	require.Equal(t, "knomit_explain", done["tu_load"]["key_arg"])
	snap := snapshotCalls(t, h, "uid:501", id)
	require.Equal(t, "Load tools", snap["tu_load"]["title"])
	require.Equal(t, "knomit_explain", snap["tu_load"]["key_arg"])
}
