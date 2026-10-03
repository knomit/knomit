package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// Scripts and recipes (knomit.learn / knomit.update) reach the same handlers
// through the script tool set, so they get the same gates. A goja-exported
// number arrives as int64; the JSON round trip makes it a number like any
// other. A newline is refused, and so is an undeclared key.
func TestScriptTools_ContextThroughTheSameGates(t *testing.T) {
	o, err := fact.ParseNewOntology([]byte(contextOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, o)
	tools := NewScriptTools(nil)
	ctx := scriptCtx(t, ri, store.Trailers{})

	learned, text, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "verdicts", "category": "t-17", "title": "Scripted verdict",
			"body": "body", "context": map[string]any{"task": "t-17", "verdict": "agree", "score": int64(1)}}},
	})
	require.False(t, isErr, text)
	file := learned["commits"].([]any)[0].(map[string]any)["file"].(string)
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	got, err := svc.Facts().ReadFact(context.Background(), "agent/test", file, nil)
	release()
	require.NoError(t, err)
	require.Contains(t, got.Content, "context: {score: 1, task: t-17, verdict: agree}")

	_, text, isErr = callScript(t, tools, ctx, "update", map[string]any{"file": file, "moment_name": "trigger:t1",
		"updates": map[string]any{"context": map[string]any{"task": "t-17\nsecond line", "verdict": "agree"}}})
	require.True(t, isErr)
	require.Contains(t, text, `key "task"`)

	_, text, isErr = callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "verdicts", "category": "t-17", "title": "Undeclared",
			"body": "body", "context": map[string]any{"task": "t-17", "verdict": "agree", "rogue": "x"}}},
	})
	require.True(t, isErr)
	require.Contains(t, text, `context key "rogue"`)
}
