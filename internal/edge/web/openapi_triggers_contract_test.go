package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenAPI_TriggersDeclared (F07 PR 2 note, taken in PR 3): the triggers
// route and its schemas declare every enum a client branches on — the
// episode, source and outcome kinds (the `do: script` kinds included), the
// trigger states and `do` values, `script` on a trigger, the statistics
// fields, `fired_at` as date-time, and (F07 PR 5) the `do: run` kinds, the
// recipe columns and `?run=<id>` — and the events route documents the
// `trigger` event's payload. Sabotage: add an outcome without the enum.
func TestOpenAPI_TriggersDeclared(t *testing.T) {
	doc := servedOpenAPI(t)
	paths := doc["paths"].(map[string]any)
	op, ok := paths["/repos/{repo}/branches/{branch}/triggers"].(map[string]any)
	require.True(t, ok, "the triggers route must be in the served spec")
	desc := op["get"].(map[string]any)["description"].(string)
	for _, want := range []string{"`do: script`", ".knomit/triggers/<script>.js", "Knomit-Trace", "Knomit-Cause", "Knomit-Trigger",
		"script_rate_per_minute", "DROPPED", "`self_caused`", "`rate-limited`", "`script-timeout`", "`script-error`", "`ran`",
		"`do: push`", "`kicked`", "not a debounce", "`{ok: true, status: \"kicked\"}`",
		"`do: run`", ".knomit/recipes/<recipe>.js", "main", "never the agent branch", "`unbound`", "`busy`", "`started`",
		"`recipe-error`", "`recipe-timeout`", "`run_id`", "`?run=<id>`", "`{ok: true, status: \"started\", id, source}`",
		"`{bound: false}`", "knomit.exec", "never a shell", "KNOMIT_RUN"} {
		require.Contains(t, desc, want)
	}
	events := paths["/repos/{repo}/branches/{branch}/events"].(map[string]any)["get"].(map[string]any)["description"].(string)
	require.Contains(t, events, "payload")
	require.Contains(t, events, "knomit.emit")

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	fire := schemas["TriggerFire"].(map[string]any)["properties"].(map[string]any)
	enumOf := func(field string) []any {
		return fire[field].(map[string]any)["enum"].([]any)
	}
	require.ElementsMatch(t, []any{"learn", "update", "retract", "due"}, enumOf("episode"))
	require.ElementsMatch(t, []any{"local", "merged", "due"}, enumOf("source"))
	require.ElementsMatch(t, []any{"emitted", "if-error", "if-timeout", "unparseable", "ran", "script-error", "script-timeout", "rate-limited", "kicked",
		"started", "busy", "recipe-error", "recipe-timeout", "done", "spawned", "delivered", "unreachable", "run"}, enumOf("outcome"))
	require.ElementsMatch(t, []any{"repo", "local"}, enumOf("recipe_source"))
	require.Contains(t, fire, "recipe_rev")
	require.Contains(t, fire, "run_id")
	require.Contains(t, fire, "message", "F5: a recipe's non-error result text")
	runRows := schemas["TriggerRunRows"].(map[string]any)
	require.ElementsMatch(t, []any{"branch", "run", "fires", "_links"}, runRows["required"].([]any),
		"?run=<id> answers ONLY the run's rows (F5)")
	var runParam map[string]any
	for _, p := range op["get"].(map[string]any)["parameters"].([]any) {
		if pm := p.(map[string]any); pm["name"] == "run" {
			runParam = pm
		}
	}
	require.NotNil(t, runParam, "?run=<id> must be declared")
	require.Equal(t, "^run-[0-9a-f]{32}$", runParam["schema"].(map[string]any)["pattern"])
	require.Equal(t, "date-time", fire["fired_at"].(map[string]any)["format"])

	trigger := schemas["Trigger"].(map[string]any)
	props := trigger["properties"].(map[string]any)
	require.ElementsMatch(t, []any{"active", "invalid", "frozen", "unsupported"}, props["state"].(map[string]any)["enum"].([]any))
	require.ElementsMatch(t, []any{"emit", "script", "push", "run"}, props["do"].(map[string]any)["enum"].([]any))
	require.Contains(t, props, "script")
	require.Contains(t, props, "recipe")
	require.ElementsMatch(t, []any{"name", "node", "on", "do", "state", "stats"}, trigger["required"].([]any))

	stats := schemas["TriggerStats"].(map[string]any)["properties"].(map[string]any)
	for _, f := range []string{"evaluations", "fires", "if_false", "if_error", "if_timeout", "unparseable", "script_error", "script_timeout", "rate_limited", "self_caused", "slow", "duration",
		"started", "busy", "unbound", "recipe_error", "recipe_timeout", "done", "spawned", "delivered", "unreachable"} {
		require.Contains(t, stats, f)
	}
}
