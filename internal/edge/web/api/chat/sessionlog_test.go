package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
	"knomit/internal/engine/store"
)

var chatIDPattern = regexp.MustCompile(`^chat-[0-9a-f]{32}$`)

// Review Focus 7: one id everywhere.
func TestSessionLog_CorrelatesWithMCPSession(t *testing.T) {
	h := newHarness(t)
	h.queue("a1",
		step{fn: func(llm.Snapshot) step {
			return toolStep("tu_b", "knomit_bind", map[string]any{"repo": "alpha"}, usage)
		}},
		step{fn: func(s llm.Snapshot) step {
			return toolStep("tu_l", "knomit_learn", learnArgs(handleFromResult(lastToolResult(s)), "Correlated write"), usage)
		}},
		textStep("done", usage))
	id := h.create("uid:501", "a1")
	require.Regexp(t, chatIDPattern, id)
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "bind then write"))
	_, asked := drive(t, h, "uid:501", id, s, nil)
	require.Equal(t, []string{"tu_l"}, approvedIDs(asked))

	rows, _, err := h.sessions.List(context.Background(), clientsessionsFilter())
	require.NoError(t, err)
	require.Len(t, rows, 1, "the chat's MCP session is the only session")
	require.Equal(t, id, rows[0].ID, "the clientsessions row id is the chat id")
	require.Equal(t, "knomit-chat", rows[0].ClientName)
	require.Equal(t, "anthropic/a1", rows[0].ClientVersion)

	lines := readLog(t, h.logDir, id)
	require.NotEmpty(t, lines)
	for _, l := range lines {
		require.Equal(t, id, l["session_id"], "every log line carries the chat id")
	}
	_, err = os.Stat(filepath.Join(h.logDir, id+".jsonl"))
	require.NoError(t, err, "the log file stem is the chat id")

	rec := callResult(t, h, "uid:501", id, "tu_l")
	var res struct {
		Commits []struct{ Hash string } `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(rec.Result), &res), rec.Result)
	require.Len(t, res.Commits, 1)
	var msg string
	require.NoError(t, h.repos.Get("alpha").WithRead(func(svc *store.Service) {
		info, err := svc.FactsRepo().CommitInfo(context.Background(), plumbing.NewHash(res.Commits[0].Hash))
		require.NoError(t, err)
		msg = info.Message
	}))
	require.Contains(t, msg, "\nKnomit-Trace: chat:"+id+"\n")
}

func TestSessionLog_RecordsRequestsTurnsToolsApprovals(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000081", "alpha")
	h.queue("claude-test-x",
		toolStep("tu_k", "knomit_learn", learnArgs(handle, "Logged write"), usage),
		textStep("logged", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write one"))
	drive(t, h, "uid:501", id, s, map[string]string{"tu_k": "allow_session"})

	lines := readLog(t, h.logDir, id)
	var types []string
	for _, l := range lines {
		types = append(types, l["type"].(string))
	}
	require.Equal(t, []string{"chat_start", "request", "approval", "tool_call", "request", "turn", "turn", "turn", "turn"}, types)
	for i, l := range lines {
		require.EqualValues(t, i+1, l["seq"], "seq counts the lines")
		require.NotEmpty(t, l["ts"])
	}

	start := lines[0]
	require.Equal(t, "anthropic", start["provider"])
	require.Equal(t, "a1", start["model"])

	req := lines[1]
	require.Equal(t, "claude-test-x", req["model"])
	require.Equal(t, map[string]any{"input": 11.0, "cache_read": 22.0, "cache_write": 33.0, "output": 44.0,
		"cache_write_5m": 0.0, "cache_write_1h": 0.0, "prompt_eval": 0.0, "estimated": false}, req["usage"])
	items := req["items"].(map[string]any)
	require.NotEmpty(t, items["tools"])
	require.Equal(t, []any{"system"}, items["system"])

	appr, call := lines[2], lines[3]
	require.Equal(t, "allow_session", appr["decision"])
	require.Equal(t, "knomit_learn", appr["tool"])
	require.Equal(t, "learn", appr["action"])
	require.Equal(t, "repo:uid-alpha", appr["pin"])
	require.Equal(t, "tu_k", appr["call_id"])
	require.Equal(t, appr["call_id"], call["call_id"], "the tool_call line is the approved call")
	require.Equal(t, "knomit_learn", call["name"])
	require.Equal(t, false, call["is_error"])

	entry := lines[5]["entry"].(map[string]any)
	require.Equal(t, "user", entry["role"])
	require.Equal(t, "assistant", lines[6]["entry"].(map[string]any)["role"])
}

func TestSessionLog_TruncatesLongResults(t *testing.T) {
	dir := t.TempDir()
	l, err := openSessionLog(dir, "chat-00000000000000000000000000000091", nil)
	require.NoError(t, err)
	long := strings.Repeat("x", 20000)
	short := strings.Repeat("y", 100)
	// A multibyte rune straddling the cut must not be split.
	wide := strings.Repeat("é", 5000) // 2 bytes each
	l.ToolCall(llm.ToolCallLog{CallID: "tu_long", Name: "knomit_query", Args: json.RawMessage(`{}`), Content: long})
	l.ToolCall(llm.ToolCallLog{CallID: "tu_short", Name: "knomit_query", Content: short})
	l.ToolCall(llm.ToolCallLog{CallID: "tu_wide", Name: "knomit_query", Content: "z" + wide})
	l.End("test")

	got := map[string]map[string]any{}
	for _, m := range readLog(t, dir, "chat-00000000000000000000000000000091") {
		if m["type"] == "tool_call" {
			got[m["call_id"].(string)] = m
		}
	}
	lg := got["tu_long"]
	require.Equal(t, true, lg["truncated"])
	require.EqualValues(t, 20000, lg["bytes"])
	require.LessOrEqual(t, len(lg["result"].(string)), 8192)
	require.Equal(t, 8192, len(lg["result"].(string)))
	sh := got["tu_short"]
	require.Equal(t, false, sh["truncated"])
	require.EqualValues(t, 100, sh["bytes"])
	require.Equal(t, short, sh["result"])
	wd := got["tu_wide"]
	require.Equal(t, true, wd["truncated"])
	require.LessOrEqual(t, len(wd["result"].(string)), 8192)
	require.NotContains(t, wd["result"].(string), "�", "the cut is at a rune boundary")
}

func TestSessionLog_FileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits; Windows reports a synthetic 0666 and NTFS ACLs are not asserted here")
	}
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000095", "alpha")
	h.queue("a1", toolStep("tu_m", "knomit_learn", learnArgs(handle, "Mode fixture"), usage), textStep("ok", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "go"))
	drive(t, h, "uid:501", id, s, nil)

	mode := func(p string) fs.FileMode {
		fi, err := os.Stat(p)
		require.NoError(t, err, p)
		return fi.Mode().Perm()
	}
	require.Equal(t, fs.FileMode(0o700), mode(h.logDir))
	require.Equal(t, fs.FileMode(0o600), mode(filepath.Join(h.logDir, id+".jsonl")))

	c := h.mgr.find(id, writer.String())
	require.Equal(t, fs.FileMode(0o700), mode(h.mgr.run))
	require.Equal(t, fs.FileMode(0o700), mode(c.store.dir))
	require.Equal(t, fs.FileMode(0o700), mode(filepath.Join(c.store.dir, "calls")))
	require.Equal(t, fs.FileMode(0o600), mode(filepath.Join(c.store.dir, "transcript.jsonl")))
	require.Equal(t, fs.FileMode(0o600), mode(filepath.Join(c.store.dir, "calls", "tu_m.json")))
}

// The secret is never logged: not in the process log, not in the session log,
// not in the temp files, not in any response or event.
func TestChat_SecretNeverLogged(t *testing.T) {
	logs := captureLogs(t)
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000096", "alpha")
	h.queue("a1", toolStep("tu_s", "knomit_learn", learnArgs(handle, "Secret fixture"), usage), textStep("ok", usage))

	var seen []string
	_, body := h.do("uid:501", http.MethodGet, "/chat-providers", nil)
	seen = append(seen, string(body))
	code, body := h.do("uid:501", http.MethodPost, "/chats", map[string]string{"provider": "anthropic", "model": "zzz"})
	require.Equal(t, http.StatusBadRequest, code)
	seen = append(seen, string(body))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	evs, _ := drive(t, h, "uid:501", id, s, nil)
	for _, e := range evs {
		seen = append(seen, string(e.Data))
	}
	_, body = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	seen = append(seen, string(body))
	_, body = h.do("uid:501", http.MethodGet, "/chats/"+id+"/calls/tu_s", nil)
	seen = append(seen, string(body))

	// Everything on disk under the chat, then the persistent log.
	c := h.mgr.find(id, writer.String())
	require.NoError(t, filepath.WalkDir(c.store.dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() {
			b, rerr := os.ReadFile(p)
			require.NoError(t, rerr)
			seen = append(seen, string(b))
		}
		return nil
	}))
	logFile, err := os.ReadFile(filepath.Join(h.logDir, id+".jsonl"))
	require.NoError(t, err)
	seen = append(seen, string(logFile))
	h.do("uid:501", http.MethodDelete, "/chats/"+id, nil)

	require.NotEmpty(t, logs.String(), "the capture saw the process log")
	require.NotContains(t, logs.String(), plainKey)
	for i, s := range seen {
		require.NotContains(t, s, plainKey, fmt.Sprintf("capture %d", i))
	}
	logFile, err = os.ReadFile(filepath.Join(h.logDir, id+".jsonl"))
	require.NoError(t, err)
	require.NotContains(t, string(logFile), plainKey)
	require.Contains(t, string(logFile), `"type":"chat_end"`)
}
