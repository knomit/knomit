package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// wantReplay is the Replay table, spelled out per (tool, action) and checked in
// both directions. Replay is read-only AND idempotent: same data, no effect on
// the session.
var wantReplay = map[[2]string]bool{
	{"knomit_query", ""}:              true,
	{"knomit_explain", ""}:            true,
	{"knomit_materialize", ""}:        true,
	{"knomit_changes", ""}:            true,
	{"knomit_skill", ""}:              true,
	{"knomit_repos", ""}:              true,
	{"knomit_experiment", "list"}:     true,
	{"knomit_bind", ""}:               false, // changes the session's binding state
	{"knomit_learn", ""}:              false,
	{"knomit_update", ""}:             false,
	{"knomit_retract", ""}:            false,
	{"knomit_review", "start"}:        false,
	{"knomit_review", "page"}:         false, // advances
	{"knomit_review", "current"}:      false, // takes the next step when nothing is outstanding
	{"knomit_review", "answer"}:       false,
	{"knomit_review", "takeover"}:     false,
	{"knomit_hypothesize", "start"}:   false,
	{"knomit_hypothesize", "current"}: false, // the same Pipeline.Current as review
	{"knomit_hypothesize", "answer"}:  false,
	{"knomit_experiment", "open"}:     false,
	{"knomit_experiment", "sync"}:     false,
	{"knomit_experiment", "commit"}:   false,
	{"knomit_experiment", "rollback"}: false,
}

func TestToolEffects_ReplayTable(t *testing.T) {
	got := map[[2]string]bool{}
	for _, e := range toolEffects {
		got[[2]string{e.Tool, e.Action}] = e.Replay == ReplayYes
	}
	require.Equal(t, wantReplay, got)
}

// TestToolEffects_EveryRowStatesReplay: a (tool, action) whose row does not
// state Replay (the zero value) fails, so a new one cannot default silently.
// toolEffects has a row for every registered (tool, action)
// (TestToolEffects_EveryRegisteredToolHasRows), so this covers each of them.
func TestToolEffects_EveryRowStatesReplay(t *testing.T) {
	require.NotEmpty(t, toolEffects)
	for _, e := range toolEffects {
		require.NotEqual(t, ReplayUnstated, e.Replay, "%s/%s does not state Replay", e.Tool, e.Action)
	}
	regs := toolRegistrations(nil)
	require.NotEmpty(t, regs)
	for _, r := range regs {
		rows := rowsFor(r.tool.Name)
		require.NotEmpty(t, rows, r.tool.Name)
		for _, row := range rows {
			require.NotEqual(t, ReplayUnstated, row.Replay, "%s/%s", row.Tool, row.Action)
		}
	}
}

func TestToolEffects_ReplayImpliesReadOnly(t *testing.T) {
	for _, e := range toolEffects {
		if e.Replay == ReplayYes {
			require.True(t, e.ReadOnly, "%s/%s is replayable but not read-only", e.Tool, e.Action)
		}
	}
}

func TestReplayable_UnlistedIsFalse(t *testing.T) {
	require.False(t, Replayable("knomit_nonesuch", json.RawMessage(`{}`)))
	require.False(t, Replayable("script_tool", nil))
	// A known tool, an action the table does not have.
	require.False(t, Replayable("knomit_experiment", json.RawMessage(`{"action":"frobnicate"}`)))
	// Arguments that do not decode.
	require.False(t, Replayable("knomit_query", json.RawMessage(`{`)))
}

func TestReplayable_FollowsTheCallsAction(t *testing.T) {
	require.True(t, Replayable("knomit_experiment", json.RawMessage(`{"action":"list"}`)))
	require.False(t, Replayable("knomit_experiment", json.RawMessage(`{"action":"commit"}`)))
	require.False(t, Replayable("knomit_review", json.RawMessage(`{"session_id":"s","page":2}`)))
	require.False(t, Replayable("knomit_review", json.RawMessage(`{"session_id":"s"}`)))
	require.False(t, Replayable("knomit_hypothesize", json.RawMessage(`{"session_id":"s","current":true}`)))
	require.True(t, Replayable("knomit_query", json.RawMessage(`{"text":"retries"}`)))
}

// TestReplayable_CursorConsumesThePage: a query, explain or materialize cursor
// dequeues the page it names, so the same call returns the next page: not
// replayable with a cursor. knomit_changes' keyset cursor and explain's
// history_cursor are stateless and replay.
func TestReplayable_CursorConsumesThePage(t *testing.T) {
	for _, tool := range []string{"knomit_query", "knomit_explain", "knomit_materialize"} {
		require.True(t, Replayable(tool, json.RawMessage(`{"limit":3}`)), tool)
		require.False(t, Replayable(tool, json.RawMessage(`{"cursor":"c1"}`)), tool)
		require.True(t, Replayable(tool, json.RawMessage(`{"cursor":""}`)), tool+": an empty cursor is a fresh call")
	}
	require.True(t, Replayable("knomit_changes", json.RawMessage(`{"cursor":"k1"}`)))
	require.True(t, Replayable("knomit_explain", json.RawMessage(`{"file":"kb/a.md","history_cursor":"h1"}`)))
}
