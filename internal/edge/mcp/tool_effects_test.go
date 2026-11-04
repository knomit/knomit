package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// registeredActions returns the values of a tool's `action` enum, or nil for a
// tool whose actions are inferred from other arguments.
func registeredActions(t mcpgo.Tool) []string {
	prop, ok := t.InputSchema.Properties["action"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	switch e := prop["enum"].(type) {
	case []string:
		out = e
	case []any:
		for _, v := range e {
			out = append(out, v.(string))
		}
	}
	return out
}

func rowsFor(tool string) []CallEffect {
	var out []CallEffect
	for _, e := range toolEffects {
		if e.Tool == tool {
			out = append(out, e)
		}
	}
	return out
}

// TestToolEffects_EveryRegisteredToolHasRows: a new tool without a row, or a
// row set that disagrees with the registration's `write` flag, fails here.
func TestToolEffects_EveryRegisteredToolHasRows(t *testing.T) {
	regs := toolRegistrations(nil)
	require.NotEmpty(t, regs)
	registered := map[string]bool{}
	multiAction := 0
	for _, r := range regs {
		name := r.tool.Name
		registered[name] = true
		rows := rowsFor(name)
		require.NotEmpty(t, rows, "%s has no row in toolEffects", name)

		anyWrite := false
		for _, row := range rows {
			if !row.ReadOnly {
				anyWrite = true
			}
		}
		require.Equal(t, r.write, anyWrite, "%s: registration write flag disagrees with its rows", name)

		// Every action of a multi-action tool has its own row.
		actions := registeredActions(r.tool)
		if len(actions) > 0 {
			multiAction++
		}
		for _, a := range actions {
			found := false
			for _, row := range rows {
				if row.Action == a {
					found = true
				}
			}
			require.True(t, found, "%s action %q has no row", name, a)
		}
	}
	require.Equal(t, 1, multiAction, "knomit_experiment is the one tool with an action enum; update this if that changes")
	// And no row names a tool that is not registered.
	for _, e := range toolEffects {
		require.True(t, registered[e.Tool], "row for unregistered tool %s", e.Tool)
	}
}

func listTools(t *testing.T, ctx context.Context) map[string]mcpgo.Tool {
	t.Helper()
	srv := NewServer("kb", nil, false, nil)
	raw, errMsg := rpcMsg(t, srv, ctx, "tools/list", map[string]any{})
	require.Empty(t, errMsg)
	var res struct {
		Tools []mcpgo.Tool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))
	out := map[string]mcpgo.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// TestToolAnnotations_ListedFromTheTable drives tools/list on both endpoint
// kinds that serve tools, so a filter that rebuilds a tool (the scoped
// knomit_bind) cannot silently drop the annotations.
func TestToolAnnotations_ListedFromTheTable(t *testing.T) {
	want := map[string][2]bool{ // readOnly, destructive
		"knomit_query":       {true, false},
		"knomit_explain":     {true, false},
		"knomit_review":      {false, true},
		"knomit_experiment":  {false, true},
		"knomit_hypothesize": {false, false},
		"knomit_bind":        {true, false},
		"knomit_learn":       {false, true},
	}
	for name, ctx := range map[string]context.Context{
		"unscoped": unscopedCtx(),
		"scoped":   repos.WithMountSession(context.Background(), "sid", "repo:uid-x"),
	} {
		t.Run(name, func(t *testing.T) {
			tools := listTools(t, ctx)
			for tool, w := range want {
				got, ok := tools[tool]
				require.True(t, ok, "%s not listed", tool)
				require.NotNil(t, got.Annotations.ReadOnlyHint, tool)
				require.NotNil(t, got.Annotations.DestructiveHint, tool)
				require.Equal(t, w[0], *got.Annotations.ReadOnlyHint, tool+" readOnlyHint")
				require.Equal(t, w[1], *got.Annotations.DestructiveHint, tool+" destructiveHint")
			}
			// Every listed tool carries both hints.
			for n, tl := range tools {
				require.NotNil(t, tl.Annotations.ReadOnlyHint, n)
				require.NotNil(t, tl.Annotations.DestructiveHint, n)
			}
		})
	}
}

func TestClassifyCall_ReviewTakeoverRow(t *testing.T) {
	var takeover, answer *CallEffect
	for i, e := range toolEffects {
		if e.Tool == "knomit_review" && e.Action == "takeover" {
			takeover = &toolEffects[i]
		}
		if e.Tool == "knomit_review" && e.Action == "answer" {
			answer = &toolEffects[i]
		}
	}
	require.NotNil(t, takeover, "explicit takeover row")
	require.NotNil(t, answer)
	require.False(t, takeover.ReadOnly)
	require.True(t, takeover.Destructive)
	require.NotSame(t, answer, takeover)
}

func TestClassifyCall_ActionFollowsDispatch(t *testing.T) {
	cases := []struct {
		name        string
		tool        string
		args        map[string]any
		action      string
		readOnly    bool
		destructive bool
		known       bool
	}{
		{"review start", "knomit_review", map[string]any{}, "start", true, false, true},
		{"review takeover", "knomit_review", map[string]any{"takeover": true}, "takeover", false, true, true},
		{"review page", "knomit_review", map[string]any{"session_id": "s", "page": float64(2)}, "page", true, false, true},
		{"review page+response is an answer", "knomit_review", map[string]any{"session_id": "s", "page": float64(2), "response": "x"}, "answer", false, true, true},
		{"review current", "knomit_review", map[string]any{"session_id": "s"}, "current", true, false, true},
		{"review answer", "knomit_review", map[string]any{"session_id": "s", "item_id": float64(3), "response": "{}"}, "answer", false, true, true},
		{"hypothesize answer", "knomit_hypothesize", map[string]any{"session_id": "s"}, "answer", false, false, true},
		{"hypothesize start", "knomit_hypothesize", map[string]any{}, "start", true, false, true},
		{"hypothesize current", "knomit_hypothesize", map[string]any{"session_id": "s", "current": true}, "current", true, false, true},
		{"experiment rollback", "knomit_experiment", map[string]any{"action": "rollback"}, "rollback", false, true, true},
		{"experiment list", "knomit_experiment", map[string]any{"action": "list"}, "list", true, false, true},
		{"experiment commit", "knomit_experiment", map[string]any{"action": "commit"}, "commit", false, false, true},
		{"experiment bogus", "knomit_experiment", map[string]any{"action": "bogus"}, "", false, false, false},
		{"experiment no action", "knomit_experiment", map[string]any{}, "", false, false, false},
		{"unknown tool", "knomit_nonexistent", map[string]any{}, "", false, false, false},
		{"single-action write", "knomit_retract", map[string]any{"file": "x"}, "", false, true, true},
		{"single-action read", "knomit_query", nil, "", true, false, true},
		{"bind", "knomit_bind", map[string]any{}, "", true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known := ClassifyCall(c.tool, c.args)
			require.Equal(t, c.known, known)
			if !known {
				return
			}
			require.Equal(t, c.tool, got.Tool)
			require.Equal(t, c.action, got.Action)
			require.Equal(t, c.readOnly, got.ReadOnly)
			require.Equal(t, c.destructive, got.Destructive)
		})
	}
}
