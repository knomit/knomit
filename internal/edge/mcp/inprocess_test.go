package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/llm"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/clientsessions"
)

const chatIDA = "chat-0123456789abcdef0123456789abcdef"
const chatIDB = "chat-fedcba9876543210fedcba9876543210"

var chatPrincipal = auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket}

func chatOpts(id string) InProcessOptions {
	return InProcessOptions{
		Principal: chatPrincipal, SessionID: id, Endpoint: "/api/v1/chats/" + id,
		ClientName: "knomit-chat", ClientVersion: "anthropic/claude-test-x",
	}
}

func listSessions(t *testing.T, st *clientsessions.Store) []clientsessions.Session {
	t.Helper()
	rows, _, err := st.List(context.Background(), clientsessions.Filter{Now: time.Now()})
	require.NoError(t, err)
	return rows
}

func callJSON(t *testing.T, s *InProcessSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	out, err := s.CallTool(context.Background(), name, raw)
	require.NoError(t, err)
	return out.Content, out.IsError
}

func bindHandle(t *testing.T, s *InProcessSession, repo string) string {
	t.Helper()
	text, isErr := callJSON(t, s, "knomit_bind", map[string]any{"repo": repo})
	require.False(t, isErr, text)
	var env struct {
		Binding string `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(text)).Decode(&env))
	require.NotEmpty(t, env.Binding)
	return env.Binding
}

func TestInProcessSession_UsesSuppliedSessionID(t *testing.T) {
	m, st, srv := bindFixture(t)
	s, _, tools, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	require.Equal(t, chatIDA, s.ID())
	rows := listSessions(t, st)
	require.Len(t, rows, 1)
	require.Equal(t, chatIDA, rows[0].ID, "the clientsessions row id is the supplied chat id, not an mcp-go generated one")
	require.NotEmpty(t, tools, "the prefix tools were loaded")
}

func TestInProcessSession_RegistersWithMarker(t *testing.T) {
	m, st, srv := bindFixture(t)
	s, instructions, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	require.Contains(t, instructions, "knomit_bind", "hook 3 is the unscoped endpoint's instructions")

	text, isErr := callJSON(t, s, "knomit_repos", map[string]any{})
	require.False(t, isErr, text)

	rows := listSessions(t, st)
	require.Len(t, rows, 1)
	r := rows[0]
	require.Equal(t, chatIDA, r.ID)
	require.Equal(t, "knomit-chat", r.ClientName)
	require.Equal(t, "anthropic/claude-test-x", r.ClientVersion)
	require.True(t, r.Initialized)
	require.Equal(t, chatPrincipal.String(), r.Principal)
	require.Equal(t, "", r.RemoteAddr+r.UserAgent, "an in-process session has no ip or user agent")
	require.GreaterOrEqual(t, r.RequestCount, 1, "the tool call was recorded through RecordRequest")
}

func TestInProcessSession_PermissionGateSeesPrincipal(t *testing.T) {
	learnArgs := func(handle string) map[string]any {
		return map[string]any{
			"binding": handle, "moment_name": "chat-write",
			"facts": []any{map[string]any{
				"topic": "gotchas", "category": "chat/inprocess", "title": "In-process write reached alpha",
				"body": "written through the chat session", "confidence": 0.8, "sources": 1,
			}},
		}
	}
	writerKey := chatPrincipal.String()

	// Read-only principal: the permission gate refuses, naming the permission.
	m, _, _ := bindFixture(t)
	ro := NewServer("kb", m, false, auth.StaticGrants{writerKey: {auth.Read: {}}})
	rs, _, tools, err := OpenInProcessSession(context.Background(), ro, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rs.Close(context.Background()) })
	for _, td := range tools {
		require.NotEqual(t, "knomit_learn", td.Name, "a read-only principal is not shown the write tool")
	}
	h := bindHandle(t, rs, "alpha")
	text, isErr := callJSON(t, rs, "knomit_learn", learnArgs(h))
	require.True(t, isErr, text)
	require.Contains(t, text, "write")
	require.Contains(t, text, writerKey)

	// Writer: the same call writes, and the fact is on alpha's agent branch.
	wo := chatOpts(chatIDB)
	ws, _, wtools, err := OpenInProcessSession(context.Background(), NewServer("kb", m, false, auth.StaticGrants{writerKey: {auth.Read: {}, auth.Write: {}}}), m, wo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close(context.Background()) })
	var sawLearn bool
	for _, td := range wtools {
		sawLearn = sawLearn || td.Name == "knomit_learn"
	}
	require.True(t, sawLearn, "a writer sees knomit_learn")
	wh := bindHandle(t, ws, "alpha")
	text, isErr = callJSON(t, ws, "knomit_learn", learnArgs(wh))
	require.False(t, isErr, text)

	alpha := m.Get("alpha")
	require.Equal(t, "agent/test", alpha.AgentBranch())
	text, isErr = callJSON(t, ws, "knomit_query", map[string]any{"binding": wh, "path": "kb/gotchas/chat"})
	require.False(t, isErr, text)
	require.Contains(t, text, "In-process write reached alpha")
}

func TestInProcessSession_BindingSetRecorded(t *testing.T) {
	m, st, srv := bindFixture(t)
	s, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	ha := bindHandle(t, s, "alpha")
	hb := bindHandle(t, s, "beta")
	text, isErr := callJSON(t, s, "knomit_query", map[string]any{"binding": ha, "path": "kb"})
	require.False(t, isErr, text)

	sets, err := st.SessionBindings(context.Background(), []string{chatIDA})
	require.NoError(t, err)
	var row *clientsessions.SessionBinding
	for i := range sets[chatIDA] {
		if sets[chatIDA][i].Handle == ha {
			row = &sets[chatIDA][i]
		}
	}
	require.NotNil(t, row, "the queried handle is in the session's binding set: %+v", sets[chatIDA])
	require.Equal(t, "repo:uid-alpha", row.Binding)

	pinA, ok := s.TargetPin(context.Background(), map[string]any{"binding": ha})
	require.True(t, ok)
	require.Equal(t, "repo:uid-alpha", pinA)
	pinB, ok := s.TargetPin(context.Background(), map[string]any{"binding": hb})
	require.True(t, ok)
	require.Equal(t, "repo:uid-beta", pinB)
	require.NotEqual(t, pinA, pinB)
	_, ok = s.TargetPin(context.Background(), map[string]any{"binding": "not-a-handle"})
	require.False(t, ok)
	_, ok = s.TargetPin(context.Background(), map[string]any{})
	require.False(t, ok)
}

// A query cursor belongs to the endpoint AND principal that minted it. Two chats
// with the same principal and everything else equal differ only in endpoint,
// and one chat cannot resume the other's cursor.
//
// Sabotage: make requestContext pass a constant endpoint instead of
// o.Endpoint and chat B resumes chat A's cursor.
func TestInProcessSession_CursorOwnedPerChat(t *testing.T) {
	m, _, srv := bindFixture(t)
	a, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	b, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	ha := bindHandle(t, a, "alpha")
	hb := bindHandle(t, b, "alpha")

	// Seed enough facts through chat A that a query pages.
	facts := make([]any, 25)
	for i := range facts {
		facts[i] = map[string]any{
			"topic": "gotchas", "category": "chat/cursor", "title": fmt.Sprintf("Cursor fact %04d", i),
			"body": fmt.Sprintf("body %04d %s", i, strings.Repeat("z", i)), "confidence": 0.8, "sources": 1,
		}
	}
	text, isErr := callJSON(t, a, "knomit_learn", map[string]any{"binding": ha, "moment_name": "seed", "facts": facts})
	require.False(t, isErr, text)

	first, isErr := callJSON(t, a, "knomit_query", map[string]any{"binding": ha, "path": "kb/gotchas/chat/cursor"})
	require.False(t, isErr, first)
	var page struct {
		Cursor *string `json:"cursor"`
		Facts  []any   `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(first), &page))
	require.NotNil(t, page.Cursor, "the query must page: %s", first)

	// Chat B, same principal and same repo, presents A's cursor.
	got, isErr := callJSON(t, b, "knomit_query", map[string]any{"binding": hb, "cursor": *page.Cursor})
	require.True(t, isErr, "another chat resumed the cursor: %s", got)
	require.Equal(t, "session expired or not found — omit cursor to start a new query", got)

	// The owner resumes it.
	own, isErr := callJSON(t, a, "knomit_query", map[string]any{"binding": ha, "cursor": *page.Cursor})
	require.False(t, isErr, own)
	require.Len(t, func() []any {
		var p struct {
			Facts []any `json:"facts"`
		}
		require.NoError(t, json.Unmarshal([]byte(own), &p))
		return p.Facts
	}(), 5, "the owner's page 2 is intact")
}

func TestInProcessSession_CloseEnds(t *testing.T) {
	m, st, srv := bindFixture(t)
	s, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	require.Nil(t, listSessions(t, st)[0].Ended, "an open session has not ended")

	require.NoError(t, s.Close(context.Background()))
	rows := listSessions(t, st)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Ended, "Close records ended_at")

	// The id is free again: the server-side session was unregistered.
	s2, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	require.NoError(t, s2.Close(context.Background()))
	require.NoError(t, s.Close(context.Background()), "Close twice is safe")
}

func TestInProcessSession_RefusesDuplicateIDAndImplementsToolHost(t *testing.T) {
	m, _, srv := bindFixture(t)
	s, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	var _ llm.ToolHost = s

	_, _, _, err = OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.Error(t, err, "a second live session under the same id must not register")

	// An unknown tool is a protocol error the loop turns into a tool result.
	_, err = s.CallTool(context.Background(), "knomit_nonesuch", json.RawMessage(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "knomit_nonesuch")
}

// A call that finishes after Close must not record: Touch clears ended_at, so
// recording would resurrect the ended row.
func TestInProcessSession_CallFinishingAfterCloseLeavesTheRowEnded(t *testing.T) {
	m, st, srv := bindFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	srv.AddTool(mcpgo.NewTool("test_block"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		close(entered)
		<-release
		return mcpgo.NewToolResultText("late"), nil
	})
	s, _, _, err := OpenInProcessSession(context.Background(), srv, m, chatOpts(chatIDA))
	require.NoError(t, err)

	var wg sync.WaitGroup
	var out llm.ToolOutcome
	var callErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, callErr = s.CallTool(context.Background(), "test_block", json.RawMessage(`{}`))
	}()
	<-entered
	before := listSessions(t, st)[0].RequestCount

	require.NoError(t, s.Close(context.Background()))
	ended := listSessions(t, st)[0].Ended
	require.NotNil(t, ended)

	close(release)
	wg.Wait()
	require.NoError(t, callErr)
	require.Equal(t, "late", out.Content, "the in-flight call still completes")

	rows := listSessions(t, st)
	require.NotNil(t, rows[0].Ended, "a late request must not bring the ended row back")
	require.True(t, ended.Equal(*rows[0].Ended), "ended_at is untouched")
	require.Equal(t, before, rows[0].RequestCount, "and it recorded nothing")
}

func TestOpenInProcessSession_RefusesAZeroPrincipal(t *testing.T) {
	m, st, srv := bindFixture(t)
	o := chatOpts(chatIDA)
	o.Principal = auth.Principal{}
	s, _, _, err := OpenInProcessSession(context.Background(), srv, m, o)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no principal")
	require.Nil(t, s)
	require.Empty(t, listSessions(t, st), "nothing was registered or recorded")
}
