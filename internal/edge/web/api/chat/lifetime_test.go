package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/pki"
)

// shortGrace shortens the stream grace for a test.
func shortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := chatStreamGrace
	chatStreamGrace = d
	t.Cleanup(func() { chatStreamGrace = orig })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func clientsessionsFilter() clientsessions.Filter { return clientsessions.Filter{Now: time.Now()} }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (h *harness) sessionRow(id string) (clientsessions.Session, bool) {
	h.t.Helper()
	rows, _, err := h.sessions.List(context.Background(), clientsessions.Filter{Now: time.Now()})
	require.NoError(h.t, err)
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return clientsessions.Session{}, false
}

func readLog(t *testing.T, dir, id string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	require.NoError(t, err)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

// Review Focus 3: a tab closed on an idle chat.
func TestChat_EndsAfterStreamGrace(t *testing.T) {
	shortGrace(t, 150*time.Millisecond)
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	require.NotNil(t, c)
	dir := c.store.dir
	require.True(t, exists(dir), "the chat has a temp dir while it lives")
	row, ok := h.sessionRow(id)
	require.True(t, ok)
	require.Nil(t, row.Ended, "the MCP session is open while the chat lives")

	s := h.attach("uid:501", id)
	time.Sleep(300 * time.Millisecond) // longer than the grace: an attached stream keeps the chat
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, "an attached stream holds the chat past the grace")
	s.close()

	eventually(t, "the chat to end", func() bool { return !exists(dir) })
	row, ok = h.sessionRow(id)
	require.True(t, ok)
	require.NotNil(t, row.Ended, "the MCP session recorded ended_at")
	code, _ = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestChat_ReconnectWithinGraceKeepsChat(t *testing.T) {
	shortGrace(t, 600*time.Millisecond)
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	s1 := h.attach("uid:501", id)
	s1.close()
	time.Sleep(150 * time.Millisecond)
	s2 := h.attach("uid:501", id) // attach asserts the first frame is "ready"
	time.Sleep(900 * time.Millisecond)
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, "the reattached stream kept the chat alive past one full grace")
	s2.close()
}

// Review Focus 3: a tab closed during an approval.
func TestChat_EndDuringApprovalUnwinds(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000071", "alpha")
	h.queue("a1", toolStep("tu_e", "knomit_learn", learnArgs(handle, "Never written"), usage), textStep("unreachable", usage))
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	c := h.mgr.find(id, writer.String())
	dir := c.store.dir

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	s.until("approval")

	turnDone := make(chan struct{})
	go func() { c.turns.Wait(); close(turnDone) }()
	code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id, nil)
	require.Equal(t, http.StatusNoContent, code)

	select {
	case <-turnDone:
	case <-time.After(time.Second):
		t.Fatal("the turn goroutine did not unwind within 1s of DELETE")
	}
	require.False(t, exists(dir), "the temp dir is gone")
	row, ok := h.sessionRow(id)
	require.True(t, ok)
	require.NotNil(t, row.Ended)
	require.NotContains(t, h.queryTitles(writer, "chat-00000000000000000000000000000072", handle), "Never written")
	code, _ = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestChat_SweepAtStartRemovesDeadRuns(t *testing.T) {
	base := filepath.Join(t.TempDir(), "chat-sessions")
	require.NoError(t, os.MkdirAll(base, 0o700))
	// A run whose owner died: files left, lock dropped.
	lock, deadRun, err := httpx.ClaimRun(base, nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(deadRun, "leftover"), []byte("x"), 0o600))
	require.NoError(t, pki.UnlockFile(lock))
	require.NoError(t, lock.Close())
	// And a run that is alive.
	liveRun, release, err := httpx.AcquireRunDir(context.Background(), base)
	require.NoError(t, err)
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	m := newManager(ctx, nil, nil, nil, base, "", nil)
	require.NotNil(t, m)
	require.False(t, exists(deadRun), "the first router build removed the dead run")
	require.True(t, exists(liveRun), "a run whose lock is held stays")
	cancel()
}

func TestChat_SessionLogOutlivesChat(t *testing.T) {
	shortGrace(t, 100*time.Millisecond)
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	dir := c.store.dir
	h.mgr.find(id, writer.String()) // still live
	eventually(t, "the chat to end by grace", func() bool { return !exists(dir) })

	lines := readLog(t, h.logDir, id)
	require.Equal(t, "chat_start", lines[0]["type"])
	last := lines[len(lines)-1]
	require.Equal(t, "chat_end", last["type"])
	require.Equal(t, "stream_grace", last["reason"])

	// A later start's sweep touches only the sessions directory.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newManager(ctx, nil, nil, nil, h.sessionDir, h.logDir, nil)
	require.True(t, exists(filepath.Join(h.logDir, id+".jsonl")), "the sweep never removes a session log")
	n, err := httpx.SweepDeadRuns(h.sessionDir)
	require.NoError(t, err)
	_ = n
	require.True(t, exists(filepath.Join(h.logDir, id+".jsonl")))
}

func TestChat_ShutdownEndsEveryChatAndRemovesTheRun(t *testing.T) {
	h := newHarness(t)
	a := h.create("uid:501", "a1")
	b := h.create("uid:1000", "a2")
	run := h.mgr.run
	require.True(t, exists(run))
	h.life()
	eventually(t, "the run to be removed", func() bool { return !exists(run) })
	for _, id := range []string{a, b} {
		row, ok := h.sessionRow(id)
		require.True(t, ok)
		require.NotNil(t, row.Ended, id)
		last := readLog(t, h.logDir, id)
		require.Equal(t, "shutdown", last[len(last)-1]["reason"])
	}
}
