package mcp

import (
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// CallEffect classifies one (tool, action) pair. This table is the single
// authority for whether a call only reads or has side effects: the MCP
// readOnlyHint / destructiveHint annotations are derived from it, and an
// approval gate in front of the tools must consult it (ClassifyCall) rather
// than keep a list of its own.
type CallEffect struct {
	Tool, Action          string // Action "" for single-action tools
	ReadOnly, Destructive bool
	// Replay says whether calling the same thing again returns the same data
	// and has no session effect (see Replay). Every row states it; the zero
	// value is "not stated" and fails TestToolEffects_EveryRowStatesReplay.
	Replay Replay
	// ReplayNotWith names arguments that make an otherwise replayable call not
	// replayable: a cursor consumes the page it names, so calling it again
	// returns the NEXT page, not the same data.
	ReplayNotWith []string
}

// Replay is whether a call may be offered back to the model as a way to get an
// earlier result again (the stub of an ended turn says so). It is stricter than
// ReadOnly: replayable means read-only AND idempotent, the same data and no
// effect on the session. knomit_bind is read-only for the knowledge base but
// changes the session's binding state; knomit_review page advances. Neither is
// replayable. Anything not in the table is not replayable.
type Replay uint8

const (
	ReplayUnstated Replay = iota // the row does not say: a test failure
	ReplayYes                    // read-only and idempotent
	ReplayNo                     // anything else
)

// Review, hypothesize and experiment actions. The strings are the table's
// vocabulary; experiment's are also the values of its `action` argument.
const (
	actStart    = "start"
	actPage     = "page"
	actCurrent  = "current"
	actAnswer   = "answer"
	actTakeover = "takeover"
)

// cursorArgs are the arguments that turn a read into a page-consuming call: a
// query, explain or materialize `cursor` dequeues the page it names, so the
// same call returns the next page. (knomit_changes' cursor is a stateless
// keyset position and explain's history_cursor is too: both replay.)
var cursorArgs = []string{"cursor"}

// toolEffects has a row for every action of every registered tool. A new tool
// or a new `action` enum value without a row fails
// TestToolEffects_EveryRegisteredToolHasRows.
var toolEffects = []CallEffect{
	{Tool: "knomit_query", ReadOnly: true, Replay: ReplayYes, ReplayNotWith: cursorArgs},
	{Tool: "knomit_explain", ReadOnly: true, Replay: ReplayYes, ReplayNotWith: cursorArgs},
	{Tool: "knomit_materialize", ReadOnly: true, Replay: ReplayYes, ReplayNotWith: cursorArgs},
	{Tool: "knomit_changes", ReadOnly: true, Replay: ReplayYes},
	{Tool: "knomit_skill", ReadOnly: true, Replay: ReplayYes},
	{Tool: "knomit_repos", ReadOnly: true, Replay: ReplayYes},
	// bind mints a handle and writes no knowledge.
	{Tool: "knomit_bind", ReadOnly: true, Replay: ReplayNo},

	{Tool: "knomit_learn", Destructive: true, Replay: ReplayNo},
	{Tool: "knomit_update", Destructive: true, Replay: ReplayNo},
	{Tool: "knomit_retract", Destructive: true, Replay: ReplayNo},

	// A start opens or resumes a pipeline session and can DISPLACE a session
	// idle past the resume window even without takeover, so it needs approval.
	{Tool: "knomit_review", Action: actStart, Replay: ReplayNo},
	// page and current only read, or advance, the caller's own session. Neither
	// is replayable: page advances, and current takes the session's next step
	// when nothing is outstanding (Pipeline.Current), so calling it again can
	// move the session.
	{Tool: "knomit_review", Action: actPage, ReadOnly: true, Replay: ReplayNo},
	{Tool: "knomit_review", Action: actCurrent, ReadOnly: true, Replay: ReplayNo},
	{Tool: "knomit_review", Action: actAnswer, Destructive: true, Replay: ReplayNo},
	// A takeover start abandons another caller's session, so it is a write
	// of its own kind, never covered by a grant for answers.
	{Tool: "knomit_review", Action: actTakeover, Destructive: true, Replay: ReplayNo},

	// A start creates a session and silently displaces an active one on its
	// branch (there is no resume rule, unlike review), so it needs approval.
	{Tool: "knomit_hypothesize", Action: actStart, Replay: ReplayNo},
	{Tool: "knomit_hypothesize", Action: actCurrent, ReadOnly: true, Replay: ReplayNo},
	// session_id without current is an answer, even with an empty response.
	{Tool: "knomit_hypothesize", Action: actAnswer, Replay: ReplayNo},

	{Tool: "knomit_experiment", Action: "list", ReadOnly: true, Replay: ReplayYes},
	{Tool: "knomit_experiment", Action: "open", Replay: ReplayNo},
	{Tool: "knomit_experiment", Action: "sync", Replay: ReplayNo},
	{Tool: "knomit_experiment", Action: "commit", Replay: ReplayNo},
	{Tool: "knomit_experiment", Action: "rollback", Destructive: true, Replay: ReplayNo},
}

// actionOf names the action a call dispatches to, or "" when the tool has
// none. An action the table does not know simply has no row. The review and
// hypothesize functions are the ones the handlers themselves dispatch on
// (reviewMode, hypothesizeMode): classification follows what the handler will
// DO, not what the caller labelled.
var actionOf = map[string]func(req mcpgo.CallToolRequest) string{
	"knomit_review":      reviewMode,
	"knomit_hypothesize": hypothesizeMode,
	"knomit_experiment": func(req mcpgo.CallToolRequest) string {
		return req.GetString("action", "")
	},
}

// multiModeTools lists, for each tool whose modes are inferred from argument
// shape, every mode its handler can dispatch to. The completeness test
// requires a row for each. knomit_experiment's modes are its `action` enum,
// which the test reads from the registration.
var multiModeTools = map[string][]string{
	"knomit_review":      ReviewModes,
	"knomit_hypothesize": HypothesizeModes,
}

// ClassifyCall classifies one call by tool name and decoded arguments.
// known=false for a tool or action not in the table: callers must treat it as
// side-effecting and never grant it.
func ClassifyCall(tool string, args map[string]any) (CallEffect, bool) {
	action := ""
	if f, multi := actionOf[tool]; multi {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		action = f(req)
	}
	for _, e := range toolEffects {
		if e.Tool == tool && e.Action == action {
			return e, true
		}
	}
	return CallEffect{}, false
}

// Replayable reports whether a call to tool with the JSON arguments args may be
// offered back to the model as a way to get its earlier result again: its row
// says Replayable and none of the row's ReplayNotWith arguments is set. A tool
// or action not in the table, and arguments that do not decode, are not
// replayable.
func Replayable(tool string, args json.RawMessage) bool {
	var m map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return false
		}
	}
	e, known := ClassifyCall(tool, m)
	if !known || e.Replay != ReplayYes {
		return false
	}
	for _, a := range e.ReplayNotWith {
		if v, set := m[a]; set && v != nil && v != "" {
			return false
		}
	}
	return true
}

// annotateFromEffects sets the tool's hints from its rows: read-only when
// every action is read-only, destructive when any action is. A tool with no
// rows is returned unchanged.
func annotateFromEffects(t mcpgo.Tool) mcpgo.Tool {
	rows := 0
	readOnly, destructive := true, false
	for _, e := range toolEffects {
		if e.Tool != t.Name {
			continue
		}
		rows++
		readOnly = readOnly && e.ReadOnly
		destructive = destructive || e.Destructive
	}
	if rows == 0 {
		return t
	}
	t.Annotations.ReadOnlyHint = &readOnly
	t.Annotations.DestructiveHint = &destructive
	return t
}
