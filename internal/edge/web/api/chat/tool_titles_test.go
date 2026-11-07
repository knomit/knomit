package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

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

// A long main argument is cut at 200 bytes, never inside a rune: here "é"
// (two bytes) sits on bytes 199-200, so the cut falls before it.
//
// SABOTAGE: labels.go, return s uncut -> the full 250-byte value comes back.
func TestBoundKeyArg_CutsAt200BytesOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("a", 199) + "é" + strings.Repeat("b", 49)
	require.Equal(t, 250, len(long))
	got := boundKeyArg(long)
	require.True(t, utf8.ValidString(got), "the cut never splits a rune")
	require.Equal(t, strings.Repeat("a", 199)+"…", got)
	require.Equal(t, "short", boundKeyArg("short"))
}

// Whether a call acts in a repo or lens (its arguments carry binding, repo or
// lens) is decided by the server and sent as in_base on the running and done
// events, the snapshot row and the call record, so the page never needs the
// arguments for it. The running event drops arguments over 16 KiB; in_base
// still rides on it. A call with none of those arguments has no in_base.
//
// SABOTAGE: chat.go, leave in_base off the running event -> the large call's
// live event has neither arguments nor in_base and this fails.
func TestChat_InBaseIsSentWithoutTheArguments(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000082", "alpha")
	big := learnArgs(handle, "big")
	big["facts"].([]any)[0].(map[string]any)["body"] = strings.Repeat("x ", maxApprovalArgs)
	h.queue("a1",
		toolStep("tu_l", "knomit_learn", big, usage),
		toolStep("tu_r", "knomit_repos", map[string]any{}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	running, done := toolCallEvents(t, evs)

	require.Equal(t, "knomit_learn", running["tu_l"]["name"])
	require.NotContains(t, running["tu_l"], "arguments", "over 16 KiB, the live event carries no arguments")
	for _, ev := range []map[string]map[string]any{running, done} {
		require.Equal(t, true, ev["tu_l"]["in_base"])
		require.Equal(t, "knomit_repos", ev["tu_r"]["name"])
		require.NotContains(t, ev["tu_r"], "in_base")
	}
	snap := snapshotCalls(t, h, "uid:501", id)
	require.Equal(t, true, snap["tu_l"]["in_base"])
	require.NotContains(t, snap["tu_r"], "in_base")
	require.True(t, callResult(t, h, "uid:501", id, "tu_l").InBase)
	require.False(t, callResult(t, h, "uid:501", id, "tu_r").InBase)
}

// The repo or lens a call's arguments name explicitly is sent as `target` on
// the running and done events, the snapshot row and the call record, so the
// page never reads it from the arguments (which the running event drops over
// 16 KiB). lens is preferred to repo, as the page always did. A call that
// names neither has no target.
//
// SABOTAGE: chat.go, leave target off the running event -> the large call's
// live event has no way to say "in alpha" and this fails.
func TestChat_TargetIsSentWithoutTheArguments(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000083", "alpha")
	big := map[string]any{"binding": handle, "repo": "alpha", "prefix": strings.Repeat("x", maxApprovalArgs+10)}
	h.queue("a1",
		toolStep("tu_c", "knomit_changes", big, usage),
		toolStep("tu_q", "knomit_query", map[string]any{"binding": handle, "text": "q"}, usage),
		toolStep("tu_x", "acme_lensed", map[string]any{"repo": "r", "lens": "L"}, usage),
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "changes"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	running, done := toolCallEvents(t, evs)

	require.NotContains(t, running["tu_c"], "arguments", "over 16 KiB, the live event carries no arguments")
	for _, ev := range []map[string]map[string]any{running, done} {
		require.Equal(t, "alpha", ev["tu_c"]["target"])
		require.Equal(t, "knomit_query", ev["tu_q"]["name"])
		require.NotContains(t, ev["tu_q"], "target")
		require.Equal(t, "L", ev["tu_x"]["target"], "lens is preferred to repo")
	}
	snap := snapshotCalls(t, h, "uid:501", id)
	require.Equal(t, "alpha", snap["tu_c"]["target"])
	require.NotContains(t, snap["tu_q"], "target")
	require.Equal(t, "alpha", callResult(t, h, "uid:501", id, "tu_c").Target)
}
