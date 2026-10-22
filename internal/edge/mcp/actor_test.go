package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

// fakeSession is a ClientSession that also carries client info, matching what
// every real transport session implements (streamable HTTP, SSE, stdio and
// in-process all satisfy SessionWithClientInfo).
type fakeSession struct {
	id   string
	info mcpgo.Implementation
}

func (f *fakeSession) Initialize()       {}
func (f *fakeSession) Initialized() bool { return true }
func (f *fakeSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return make(chan mcpgo.JSONRPCNotification, 1)
}
func (f *fakeSession) SessionID() string                     { return f.id }
func (f *fakeSession) GetClientInfo() mcpgo.Implementation   { return f.info }
func (f *fakeSession) SetClientInfo(ci mcpgo.Implementation) { f.info = ci }
func (f *fakeSession) GetClientCapabilities() mcpgo.ClientCapabilities {
	return mcpgo.ClientCapabilities{}
}
func (f *fakeSession) SetClientCapabilities(mcpgo.ClientCapabilities) {}

var _ mcpserver.SessionWithClientInfo = (*fakeSession)(nil)

// sessionOnly is a ClientSession WITHOUT client info — the shape a session
// takes when the caller never ran `initialize` against this process.
type sessionOnly struct{ id string }

func (s *sessionOnly) Initialize()       {}
func (s *sessionOnly) Initialized() bool { return true }
func (s *sessionOnly) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return make(chan mcpgo.JSONRPCNotification, 1)
}
func (s *sessionOnly) SessionID() string { return s.id }

var _ mcpserver.ClientSession = (*sessionOnly)(nil)

func withSession(s mcpserver.ClientSession) context.Context {
	return mcpserver.NewMCPServer("t", "1").WithContext(context.Background(), s)
}

// The composed handle is scheme-prefixed so a reader can see what KIND of
// claim it is from the value alone — `mcp-session:` says "the caller told us
// this", which is the whole point of knomit#123's honesty requirement. The
// client clause is an enrichment layered on the id, never a replacement for it.
func TestActorFromRequest_Composition(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess mcpserver.ClientSession
		want string
	}{
		{
			name: "session id plus client info",
			sess: &fakeSession{id: "sid-1", info: mcpgo.Implementation{Name: "claude-code", Version: "1.2.3"}},
			want: "mcp-session:sid-1 client:claude-code/1.2.3",
		},
		{
			// Measured against the real transport: a call arriving under a
			// session this process never saw `initialize` for still carries an
			// id, and carries NO client info. The id alone must still be
			// recorded — it is the join key to the HTTP log.
			name: "session id, no client info recorded",
			sess: &fakeSession{id: "sid-2"},
			want: "mcp-session:sid-2",
		},
		{
			name: "session that cannot carry client info at all",
			sess: &sessionOnly{id: "sid-3"},
			want: "mcp-session:sid-3",
		},
		{
			name: "client name without a version",
			sess: &fakeSession{id: "sid-4", info: mcpgo.Implementation{Name: "someclient"}},
			want: "mcp-session:sid-4 client:someclient",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := actorFromRequest(withSession(tc.sess), mcpgo.CallToolRequest{})
			require.Equal(t, tc.want, got)
		})
	}
}

// No MCP session at all: an in-process caller. Honest absence — NOT an invented
// "unknown" sentinel, which would be indistinguishable from a real handle that
// happened to say "unknown".
func TestActorFromRequest_NoSessionIsEmpty(t *testing.T) {
	require.Equal(t, "", actorFromRequest(context.Background(), mcpgo.CallToolRequest{}))
}

// A session id that sanitizes away to nothing yields no handle rather than a
// bare scheme prefix `mcp-session:`, which would read as a recorded attribution
// while naming nobody.
func TestActorFromRequest_UnusableSessionIDIsEmpty(t *testing.T) {
	got := actorFromRequest(withSession(&sessionOnly{id: "\n\t  "}), mcpgo.CallToolRequest{})
	require.Equal(t, "", got)
}

// Every byte of the handle is caller-supplied. It lands in a DB column and in
// log lines, so it is capped and stripped of control characters — a newline in
// particular would split one log line into two, one of them attacker-shaped.
func TestActorFromRequest_CallerSuppliedBytesAreBounded(t *testing.T) {
	long := strings.Repeat("x", actorMaxLen*3)
	got := actorFromRequest(withSession(&fakeSession{
		id:   long,
		info: mcpgo.Implementation{Name: "ev\nil na\tme", Version: long},
	}), mcpgo.CallToolRequest{})

	require.NotContains(t, got, "\n", "a newline would forge a second log line")
	require.NotContains(t, got, "\t")
	require.Contains(t, got, "client:evilname/", "control chars are dropped, the rest is kept")

	// Each component is capped independently, so the id being over-long cannot
	// crowd the client clause out of the value.
	id := strings.TrimPrefix(strings.SplitN(got, " ", 2)[0], "mcp-session:")
	require.Len(t, id, actorMaxLen, "the id component is capped, not merely shortened somewhere")
	require.Less(t, len(got), actorMaxLen*3, "the whole handle stays bounded")
}
