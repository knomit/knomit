package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A provider whose adapter sets StablePrefix gets no ToolWindow, so its requests
// are what the Builder holds: every tool, the whole history, nothing projected.
// The pinned digest was taken from the code before adaptive tools existed, so
// it falsifies any drift in the bytes an Anthropic request carries.
func TestLoop_StablePrefixRequestsAreByteIdentical(t *testing.T) {
	m := &fakeModel{
		caps: Caps{Tools: true, MaxBreakpoints: 4, ContextWindow: 100000, StablePrefix: true},
		steps: []fakeStep{
			{resp: toolResp("tu1", "knomit_repos", `{"a":1}`, Usage{Input: 50, Output: 5})},
			{resp: textResp("done", Usage{Input: 80, Output: 5})},
		},
	}
	h := &fakeHost{out: map[string]ToolOutcome{"knomit_repos": {Content: "repo list"}}}
	l, b := newLoop(t, m, h)
	for _, n := range []string{"knomit_query", "knomit_learn"} {
		require.NoError(t, b.Add(Item{ID: "tool." + n, Hook: HookTools, Tool: &ToolDef{Name: n, Description: "d " + n, Schema: json.RawMessage(`{"type":"object"}`)}}))
	}
	_, err := l.RunTurn(context.Background(), "hello")
	require.NoError(t, err)
	require.Len(t, m.snaps, 2)

	sum := sha256.New()
	for i, s := range m.snaps {
		require.Len(t, s.Hooks[HookTools], 3, "request %d must carry every tool", i)
		raw, err := json.Marshal(renderAnthropic(s, "claude-test-x", AnthropicOptions{}))
		require.NoError(t, err)
		sum.Write(raw)
	}
	require.Equal(t, "c4c552b25133d602495659692ef77a04bdca410b3d9c8bb8fdddf43c0f9f6a24", hex.EncodeToString(sum.Sum(nil)))
}
