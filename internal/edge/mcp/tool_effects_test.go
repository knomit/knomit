package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/authz"
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

		// Every mode of a multi-mode tool has its own row. The mode set is the
		// handler's own: the `action` enum for experiment, ReviewModes /
		// HypothesizeModes (the lists the handlers dispatch on) for the rest.
		modes := registeredActions(r.tool)
		if len(modes) > 0 {
			multiAction++
		}
		if listed, ok := multiModeTools[name]; ok {
			require.Empty(t, modes, "%s has both an action enum and a mode list", name)
			modes = listed
			multiAction++
		}
		require.Equal(t, len(modes) > 0, len(rows) > 1 || rows[0].Action != "",
			"%s: rows are mode-keyed exactly when the tool has an enumerable mode set; a multi-mode tool must be listed in multiModeTools", name)
		for _, a := range modes {
			found := false
			for _, row := range rows {
				if row.Action == a {
					found = true
				}
			}
			require.True(t, found, "%s mode %q has no row", name, a)
		}
		// And no row names a mode the tool cannot dispatch to.
		for _, row := range rows {
			if row.Action != "" {
				require.Contains(t, modes, row.Action, "%s row for a mode the handler does not have", name)
			}
		}
	}
	require.Equal(t, 3, multiAction, "review, hypothesize and experiment are the multi-mode tools; update this if that changes")
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

// TestModes_ListsMatchDispatch: the mode lists the completeness test relies on
// equal what reviewMode / hypothesizeMode can return over every argument
// shape, so a mode added to the dispatch without being listed fails here.
func TestModes_ListsMatchDispatch(t *testing.T) {
	for _, c := range []struct {
		name  string
		fn    func(mcpgo.CallToolRequest) string
		modes []string
	}{
		{"review", reviewMode, ReviewModes},
		{"hypothesize", hypothesizeMode, HypothesizeModes},
	} {
		t.Run(c.name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, session := range []string{"", "s"} {
				for _, response := range []string{"", "r"} {
					for _, page := range []float64{0, 2} {
						for _, takeover := range []bool{false, true} {
							for _, current := range []bool{false, true} {
								req := mcpgo.CallToolRequest{}
								req.Params.Arguments = map[string]any{
									"session_id": session, "response": response,
									"page": page, "takeover": takeover, "current": current,
								}
								seen[c.fn(req)] = true
							}
						}
					}
				}
			}
			var got []string
			for m := range seen {
				got = append(got, m)
			}
			require.ElementsMatch(t, c.modes, got)
		})
	}
}

// TestToolEffects_ModeClassifications pins WHICH classification every
// review and hypothesize mode has, so a flipped row fails by name.
func TestToolEffects_ModeClassifications(t *testing.T) {
	type cls struct{ readOnly, destructive bool }
	want := map[string]map[string]cls{
		"knomit_review": {
			actStart:    {false, false}, // can displace an idle session
			actTakeover: {false, true},
			actPage:     {true, false},
			actCurrent:  {true, false},
			actAnswer:   {false, true},
		},
		"knomit_hypothesize": {
			actStart:   {false, false}, // displaces an active session silently
			actCurrent: {true, false},
			actAnswer:  {false, false},
		},
	}
	for tool, modes := range want {
		require.Len(t, modes, len(multiModeTools[tool]), tool)
		for mode, w := range modes {
			var found *CallEffect
			for i, e := range toolEffects {
				if e.Tool == tool && e.Action == mode {
					found = &toolEffects[i]
				}
			}
			require.NotNil(t, found, "%s %s", tool, mode)
			require.Equal(t, w.readOnly, found.ReadOnly, "%s %s ReadOnly", tool, mode)
			require.Equal(t, w.destructive, found.Destructive, "%s %s Destructive", tool, mode)
		}
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
		{"review start", "knomit_review", map[string]any{}, "start", false, false, true},
		{"review takeover", "knomit_review", map[string]any{"takeover": true}, "takeover", false, true, true},
		{"review page", "knomit_review", map[string]any{"session_id": "s", "page": float64(2)}, "page", true, false, true},
		{"review page+response is an answer", "knomit_review", map[string]any{"session_id": "s", "page": float64(2), "response": "x"}, "answer", false, true, true},
		{"review current", "knomit_review", map[string]any{"session_id": "s"}, "current", true, false, true},
		{"review answer", "knomit_review", map[string]any{"session_id": "s", "item_id": float64(3), "response": "{}"}, "answer", false, true, true},
		{"hypothesize answer", "knomit_hypothesize", map[string]any{"session_id": "s"}, "answer", false, false, true},
		{"hypothesize start", "knomit_hypothesize", map[string]any{}, "start", false, false, true},
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

// Every (tool, action) row declares the permission a call needs (unified
// authn/z P1): the MCP half of the permission registry. Undeclared is denied.
func TestToolEffects_EveryRowHasPermission(t *testing.T) {
	for _, e := range toolEffects {
		if _, err := authz.ParsePermission(string(e.Perm)); err != nil {
			t.Errorf("%s %q: Perm %q is not a permission: %v", e.Tool, e.Action, e.Perm, err)
		}
	}
}

// ReadOnly <=> kb:read, with no exceptions: a read-only call needs only
// kb:read and anything with a side effect needs more. This is also what moves
// the read actions of review, hypothesize and experiment off kb:write (§3).
func TestToolEffects_ReadOnlyRowsAreKBRead(t *testing.T) {
	for _, e := range toolEffects {
		if e.ReadOnly != (e.Perm == authz.KBRead) {
			t.Errorf("%s %q: ReadOnly=%v but Perm=%q", e.Tool, e.Action, e.ReadOnly, e.Perm)
		}
	}
}

func TestPermissionFor_ReadActionsAreKBRead(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want authz.Permission
	}{
		{"knomit_query", map[string]any{"text": "x"}, authz.KBRead},
		{"knomit_explain", map[string]any{}, authz.KBRead},
		{"knomit_bind", map[string]any{}, authz.KBRead},
		{"knomit_experiment", map[string]any{"action": "list"}, authz.KBRead},
		{"knomit_review", map[string]any{"session_id": "s", "page": float64(2)}, authz.KBRead},
		{"knomit_review", map[string]any{"session_id": "s"}, authz.KBRead},
		{"knomit_hypothesize", map[string]any{"session_id": "s", "current": true}, authz.KBRead},

		{"knomit_experiment", map[string]any{"action": "open"}, authz.KBWrite},
		{"knomit_experiment", map[string]any{"action": "sync"}, authz.KBWrite},
		{"knomit_experiment", map[string]any{"action": "commit"}, authz.KBWrite},
		{"knomit_experiment", map[string]any{"action": "rollback"}, authz.KBWrite},
		{"knomit_review", map[string]any{}, authz.KBWrite},
		{"knomit_review", map[string]any{"session_id": "s", "item_id": float64(3), "response": "{}"}, authz.KBWrite},
		{"knomit_review", map[string]any{"takeover": true}, authz.KBWrite},
		{"knomit_hypothesize", map[string]any{}, authz.KBWrite},
		{"knomit_hypothesize", map[string]any{"session_id": "s"}, authz.KBWrite},
		{"knomit_learn", map[string]any{}, authz.KBWrite},
		{"knomit_update", map[string]any{}, authz.KBWrite},
		{"knomit_retract", map[string]any{}, authz.KBWrite},
	}
	for _, c := range cases {
		got, ok := PermissionFor(c.tool, c.args)
		if !ok || got != c.want {
			t.Errorf("PermissionFor(%s, %v) = %q, %v; want %q", c.tool, c.args, got, ok, c.want)
		}
	}
	// A tool or action the table does not know is denied, not defaulted.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"knomit_nope", nil},
		{"knomit_experiment", map[string]any{"action": "explode"}},
		{"knomit_experiment", map[string]any{}},
	} {
		if p, ok := PermissionFor(c.tool, c.args); ok {
			t.Errorf("PermissionFor(%s, %v) = %q, want denied", c.tool, c.args, p)
		}
	}
}
