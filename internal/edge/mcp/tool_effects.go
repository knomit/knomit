package mcp

import (
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
}

// Review, hypothesize and experiment actions. The strings are the table's
// vocabulary; experiment's are also the values of its `action` argument.
const (
	actStart    = "start"
	actPage     = "page"
	actCurrent  = "current"
	actAnswer   = "answer"
	actTakeover = "takeover"
)

// toolEffects has a row for every action of every registered tool. A new tool
// or a new `action` enum value without a row fails
// TestToolEffects_EveryRegisteredToolHasRows.
var toolEffects = []CallEffect{
	{Tool: "knomit_query", ReadOnly: true},
	{Tool: "knomit_explain", ReadOnly: true},
	{Tool: "knomit_materialize", ReadOnly: true},
	{Tool: "knomit_changes", ReadOnly: true},
	{Tool: "knomit_skill", ReadOnly: true},
	{Tool: "knomit_repos", ReadOnly: true},
	// bind mints a handle and writes no knowledge.
	{Tool: "knomit_bind", ReadOnly: true},

	{Tool: "knomit_learn", Destructive: true},
	{Tool: "knomit_update", Destructive: true},
	{Tool: "knomit_retract", Destructive: true},

	// A start opens or resumes a pipeline session and can DISPLACE a session
	// idle past the resume window even without takeover, so it needs approval.
	{Tool: "knomit_review", Action: actStart},
	// page and current only read, or advance, the caller's own session.
	{Tool: "knomit_review", Action: actPage, ReadOnly: true},
	{Tool: "knomit_review", Action: actCurrent, ReadOnly: true},
	{Tool: "knomit_review", Action: actAnswer, Destructive: true},
	// A takeover start abandons another caller's session, so it is a write
	// of its own kind, never covered by a grant for answers.
	{Tool: "knomit_review", Action: actTakeover, Destructive: true},

	// A start creates a session and silently displaces an active one on its
	// branch (there is no resume rule, unlike review), so it needs approval.
	{Tool: "knomit_hypothesize", Action: actStart},
	{Tool: "knomit_hypothesize", Action: actCurrent, ReadOnly: true},
	// session_id without current is an answer, even with an empty response.
	{Tool: "knomit_hypothesize", Action: actAnswer},

	{Tool: "knomit_experiment", Action: "list", ReadOnly: true},
	{Tool: "knomit_experiment", Action: "open"},
	{Tool: "knomit_experiment", Action: "sync"},
	{Tool: "knomit_experiment", Action: "commit"},
	{Tool: "knomit_experiment", Action: "rollback", Destructive: true},
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
