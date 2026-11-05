package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// ctxModel is a scripted model that also reports how its window was resolved.
// The values differ from every default so a test that passes read them.
type ctxModel struct {
	*scripted
	window int
	how    string
	spill  bool
}

func (m *ctxModel) ContextInfo() (int, string, bool) { return m.window, m.how, m.spill }

func (h *harness) queueCtx(name string, window int, how string, spill bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.models = append(h.models, &ctxModel{scripted: &scripted{name: name}, window: window, how: how, spill: spill})
}

func TestSessionLog_StartRecordsTheContextWindowAndHow(t *testing.T) {
	h := newHarness(t)
	h.queueCtx("a1", 45056, "memory", false)
	id := h.create("uid:501", "a1")

	lines := readLog(t, h.logDir, id)
	require.Equal(t, "chat_start", lines[0]["type"])
	require.EqualValues(t, 45056, lines[0]["context_window"])
	require.Equal(t, "memory", lines[0]["context_window_how"])

	// A model that does not report how falls back to its Caps window, no "how".
	id2 := h.create("uid:501", "a2")
	l2 := readLog(t, h.logDir, id2)[0]
	require.EqualValues(t, 1_000_000, l2["context_window"])
	require.Equal(t, "", l2["context_window_how"])
}

func TestChat_ContextEndpointReportsWindowHowAndSpill(t *testing.T) {
	h := newHarness(t)
	h.queueCtx("a1", 12288, "config", true)
	id := h.create("uid:501", "a1")

	code, body := h.do("uid:501", http.MethodGet, "/chats/"+id+"/context", nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var got struct {
		Window int    `json:"window"`
		How    string `json:"how"`
		Spill  bool   `json:"spill"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, 12288, got.Window)
	require.Equal(t, "config", got.How)
	require.True(t, got.Spill)

	// Another principal's chat is not visible.
	code, _ = h.do("uid:1000", http.MethodGet, "/chats/"+id+"/context", nil)
	require.Equal(t, http.StatusNotFound, code)

	// The chat view links to it.
	code, body = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"context":{"href":"/api/v1/chats/`+id+`/context"}`)
}
