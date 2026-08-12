package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"knomit/internal/obs/reqinfo"
)

func callToolMessage(name string) json.RawMessage {
	return json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`)
}

// The HTTP slow-request log names the MCP tool that was slow. That name is only
// known once the JSON-RPC body is parsed, so the BeforeCallTool hook writes it
// back into the annotation the web middleware left in the request context.
//
// The tool named here is deliberately one that is not registered: the hook runs
// ahead of dispatch, so the call resolves to a "tool not found" error instead of
// reaching a handler that would need a repo binding. That is also the behaviour
// we want in the log — the warning reports what the client ASKED for, whether or
// not the tool exists.
func TestBeforeCallToolRecordsToolNameInReqInfo(t *testing.T) {
	s := NewServer("kb", nil, false)
	ctx, info := reqinfo.NewContext(context.Background())

	s.HandleMessage(ctx, callToolMessage("knomit_no_such_tool"))

	if got := info.Tool(); got != "knomit_no_such_tool" {
		t.Errorf("Tool() = %q, want %q", got, "knomit_no_such_tool")
	}
}

// stdio sessions and in-process callers carry no annotation. The hook must be a
// no-op there, never a panic.
func TestBeforeCallToolWithoutReqInfoDoesNotPanic(t *testing.T) {
	s := NewServer("kb", nil, false)
	s.HandleMessage(context.Background(), callToolMessage("knomit_no_such_tool"))
}
