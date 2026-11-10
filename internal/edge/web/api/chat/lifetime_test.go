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

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/llm"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/pki"
)

// idleWait is how long the lifetime tests leave a chat with no page stream
// before they look at it again. Chats used to end 60 s after their owner's
// last page stream closed (or 60 s after creation with none attached); the
// sabotage for these tests puts that clock back at a fraction of idleWait.
const idleWait = 400 * time.Millisecond

// logTypes lists the record types of a chat's session log, in order.
func logTypes(t *testing.T, dir, id string) []string {
	t.Helper()
	var out []string
	for _, l := range readLog(t, dir, id) {
		out = append(out, l["type"].(string))
	}
	return out
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

// A tab closed (or a laptop asleep, or a hidden desktop window whose stream
// Wails reaped) on an idle chat: the chat stays, keeps working with no page
// attached, and ends only on DELETE, which still does every part of the end.
//
// SABOTAGE: put the stream grace back (detachPage starts a clock on the last
// close that ends the chat) with a clock shorter than idleWait -> the chat is
// gone from GET /chats and this fails.
func TestChat_OutlivesItsPageStream(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("first", usage), textStep("second", usage))
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	require.NotNil(t, c)
	dir := c.store.dir

	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	untilChat(s, id, "turn_done")
	s.close()
	h.waitDetached(writer.String())
	time.Sleep(idleWait)

	require.Equal(t, "idle", h.row("uid:501", id).State, "GET /chats still lists the chat with no page stream")
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code)
	require.True(t, exists(dir), "the temp dir stays while the chat lives")
	sess, ok := h.sessionRow(id)
	require.True(t, ok)
	require.Nil(t, sess.Ended, "the MCP session stays open while the chat lives")
	require.NotContains(t, logTypes(t, h.logDir, id), "chat_end")

	// A turn with no page attached runs; its end is parked for the next page.
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "two"))
	eventually(t, "the second turn to finish", func() bool {
		r := h.row("uid:501", id)
		return r.State == "idle" && r.Turns == 2
	})

	code, _ = h.do("uid:501", http.MethodDelete, "/chats/"+id, nil)
	require.Equal(t, http.StatusNoContent, code)
	require.False(t, exists(dir), "DELETE removes the temp dir")
	sess, ok = h.sessionRow(id)
	require.True(t, ok)
	require.NotNil(t, sess.Ended, "DELETE records ended_at on the MCP session")
	lines := readLog(t, h.logDir, id)
	require.Equal(t, "chat_end", lines[len(lines)-1]["type"])
	require.Equal(t, "deleted", lines[len(lines)-1]["reason"])
	code, _ = h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusNotFound, code)
}

// A chat created while no page stream of its owner is attached (a client that
// creates over the API, or a page whose stream was dropped) lives too.
//
// SABOTAGE: put the creation clock back (create starts the grace when the
// owner has no page stream) with a clock shorter than idleWait -> the chat is
// 404 and this fails.
func TestChat_CreatedWithNoPageStreamLives(t *testing.T) {
	h := newHarness(t)
	h.queue("a1", textStep("hello", usage))
	id := h.create("uid:501", "a1")
	time.Sleep(idleWait)
	code, _ := h.do("uid:501", http.MethodGet, "/chats/"+id, nil)
	require.Equal(t, http.StatusOK, code, "no page stream ever attached, and the chat lives")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "hi"))
	eventually(t, "the turn to finish", func() bool { return h.row("uid:501", id).Turns == 1 && h.row("uid:501", id).State == "idle" })
}

// A page that comes back (a reload, a woken laptop, a desktop window whose
// stream was reaped and reopened) gets ready, finds the chat in GET /chats with
// its row state, gets the turn end it missed, and its next turn's chat_state.
//
// SABOTAGE: put the stream grace back with a clock shorter than idleWait ->
// the chat is not listed and this fails.
func TestChat_ReattachingPageSeesTheChatAndItsState(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.queue("a1", step{fn: func(llm.Snapshot) step { <-release; return textStep("late", usage) }}, textStep("again", usage))
	id := h.create("uid:501", "a1")
	s := h.attachPage("uid:501")
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "one"))
	untilChat(s, id, "chat_state")
	s.close()
	h.waitDetached(writer.String())
	close(release) // the turn ends while no page is attached
	eventually(t, "the turn to end", func() bool { return h.row("uid:501", id).State == "idle" })
	time.Sleep(idleWait)

	s2 := h.attachPage("uid:501") // asserts the first frame is ready
	r := h.row("uid:501", id)
	require.Equal(t, "idle", r.State)
	require.Equal(t, 1, r.Turns)
	require.Equal(t, "one", r.Title)
	missed := s2.next("missed end", 3*time.Second)
	require.Equal(t, "turn_done", missed.Name)
	require.Equal(t, id, chatOfEvent(missed))

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "two"))
	st := untilChat(s2, id, "chat_state")[0]
	var body rowState
	require.NoError(t, json.Unmarshal(st.Data, &body))
	require.Equal(t, 2, body.Turns, "the reattached stream carries the chat's chat_state")
	untilChat(s2, id, "turn_done")
}

// A turn waiting for approval when its page goes away keeps waiting: nothing
// times the approval out, the chat lists as awaiting_approval, and a page that
// comes back can answer it and the turn completes.
//
// SABOTAGE: put the stream grace back with a clock shorter than idleWait ->
// ending the chat cancels the turn, the approval is gone (404) and this fails.
func TestChat_ApprovalWaitsWithNoPageAttached(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000081", "alpha")
	h.queue("a1", toolStep("tu_w", "knomit_learn", learnArgs(handle, "Written after the page came back"), usage), textStep("done", usage))
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write"))
	s.until("approval")
	s.close()
	h.waitDetached(writer.String())
	time.Sleep(idleWait)

	require.Equal(t, "awaiting_approval", h.row("uid:501", id).State)
	c.mu.Lock()
	running, pending := c.running, len(c.pending)
	c.mu.Unlock()
	require.True(t, running, "the turn is still running, blocked in Decide")
	require.Equal(t, 1, pending)

	s2 := h.attach("uid:501", id)
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_w", "allow"))
	untilChat(s2, id, "turn_done")
	require.Contains(t, h.queryTitles(writer, "chat-00000000000000000000000000000082", handle), "Written after the page came back")
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
	m := newManager(ctx, nil, nil, nil, nil, base, "", nil)
	require.NotNil(t, m)
	require.False(t, exists(deadRun), "the first router build removed the dead run")
	require.True(t, exists(liveRun), "a run whose lock is held stays")
	cancel()
}

func TestChat_SessionLogOutlivesChat(t *testing.T) {
	h := newHarness(t)
	id := h.create("uid:501", "a1")
	c := h.mgr.find(id, writer.String())
	dir := c.store.dir
	code, _ := h.do("uid:501", http.MethodDelete, "/chats/"+id, nil)
	require.Equal(t, http.StatusNoContent, code)
	require.False(t, exists(dir))

	lines := readLog(t, h.logDir, id)
	require.Equal(t, "chat_start", lines[0]["type"])
	last := lines[len(lines)-1]
	require.Equal(t, "chat_end", last["type"])
	require.Equal(t, "deleted", last["reason"])

	// A later start's sweep touches only the sessions directory.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newManager(ctx, nil, nil, nil, nil, h.sessionDir, h.logDir, nil)
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

// A chat idle longer than the session reaper's tool idle TTL (15 min in
// production, session.tool_idle_ttl) still works on its next turn: the reaper
// deletes the paging session the chat's earlier query opened, and the chat's
// next query, through the same binding handle, answers and pages as before.
// Only the reaped cursor itself is gone, as it is for any MCP client.
//
// The TTL is shortened by calling the reaper's own pass (ReapIdleSessions)
// with a 1 ms TTL (last_used_at has second precision, hence the sleep); the
// test asserts the pass deleted at least one session, so it cannot pass with
// a reaper that reached nothing.
func TestChat_IdleChatSurvivesTheToolSessionReaper(t *testing.T) {
	h := newHarness(t)
	handle := h.bind(writer, "chat-00000000000000000000000000000091", "alpha")
	learn := learnArgs(handle, "Idle chat fact one")
	learn["facts"] = append(learn["facts"].([]any), map[string]any{
		"topic": "gotchas", "category": "chat/api", "title": "Idle chat fact two",
		"body": "the second fact, so a one-row query pages", "confidence": 0.8, "sources": 1,
	})
	query := map[string]any{"binding": handle, "path": "kb/gotchas/chat", "limit": 1}
	var oldCursor string
	m := h.queue("a1",
		toolStep("tu_l", "knomit_learn", learn, usage),
		toolStep("tu_q1", "knomit_query", query, usage),
		textStep("first", usage),
		toolStep("tu_q2", "knomit_query", query, usage),
		step{fn: func(llm.Snapshot) step {
			return toolStep("tu_q3", "knomit_query", map[string]any{"binding": handle, "cursor": oldCursor}, usage)
		}},
		textStep("second", usage),
	)
	id := h.create("uid:501", "a1")
	s := h.attach("uid:501", id)
	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "write then read"))
	s.until("approval")
	require.Equal(t, http.StatusNoContent, h.approve("uid:501", id, "tu_l", "allow"))
	untilChat(s, id, "turn_done")
	s.close()
	h.waitDetached(writer.String())
	oldCursor = cursorOf(t, lastToolResult(m.requests()[2]))
	require.NotEmpty(t, oldCursor, "the first query paged")

	time.Sleep(1100 * time.Millisecond)
	reaped := 0
	h.repos.ForEach(func(_ string, ri *repos.RepoInstance) {
		_ = ri.WithRead(func(svc *store.Service) {
			n, err := svc.ReapIdleSessions(context.Background(), time.Millisecond, time.Millisecond)
			require.NoError(t, err)
			reaped += n
		})
	})
	require.Positive(t, reaped, "the reaper pass deleted the chat's idle paging session")

	require.Equal(t, http.StatusAccepted, h.turn("uid:501", id, "read again"))
	eventually(t, "the second turn to finish", func() bool {
		r := h.row("uid:501", id)
		return r.State == "idle" && r.Turns == 2
	})
	reqs := m.requests()
	require.Len(t, reqs, 6)
	fresh := lastToolResult(reqs[4])
	require.Contains(t, fresh, "Idle chat fact", "the query after the reap answered through the same handle")
	require.NotEmpty(t, cursorOf(t, fresh), "and paged: a new paging session was opened")
	require.Contains(t, lastToolResult(reqs[5]), "session expired", "only the reaped cursor is gone, with the error any client gets")
}

// cursorOf reads the cursor out of a knomit_query result ("" when none).
func cursorOf(t *testing.T, content string) string {
	t.Helper()
	var env struct {
		Cursor string `json:"cursor"`
	}
	_ = json.NewDecoder(strings.NewReader(content)).Decode(&env)
	return env.Cursor
}
