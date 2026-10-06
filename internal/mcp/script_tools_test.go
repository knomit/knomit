package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// The script tool set (F07 PR 3), handler half: the SAME closures the MCP
// server registers, called with the ctx the script host builds — an explicit
// binding on the agent branch, the trailer set, a deadline — and nothing
// else. What the host refuses on its own (a `.knomit/` write) the tool still
// allows here, which is what proves the refusal is the host's.

// scriptCtx is the ctx the host hands the tool set.
func scriptCtx(t *testing.T, ri *repos.RepoInstance, tr store.Trailers) context.Context {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "agent/test"))
	return store.WithTrailers(ctx, tr)
}

func callScript(t *testing.T, tools repos.ScriptTools, ctx context.Context, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	text, isErr, err := tools.Call(ctx, tool, args)
	require.NoError(t, err)
	var out map[string]any
	if !isErr && text != "" {
		require.NoError(t, json.Unmarshal([]byte(text), &out), "the tool's result is JSON: %s", text)
	}
	return out, text, isErr
}

func messageOf(t *testing.T, ri *repos.RepoInstance, hash string) string {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()
	info, err := svc.Triggers().CommitInfo(context.Background(), plumbing.NewHash(hash))
	require.NoError(t, err)
	return info.Message
}

// HostFunctionsReachTheHandlers: learn, update, retract, query and explain
// answer with the tool's own result shapes; an unknown tool is an error.
// Sabotage: route a name to the wrong handler.
func TestScriptTools_HostFunctionsReachTheHandlers(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	tools := NewScriptTools(nil)
	ctx := scriptCtx(t, ri, store.Trailers{})

	learned, _, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "architecture", "category": "scripts/host", "title": "Learned by a script",
			"body": "body", "confidence": 0.8, "sources": 1}},
	})
	require.False(t, isErr)
	commits := learned["commits"].([]any)
	require.Len(t, commits, 1)
	file := commits[0].(map[string]any)["file"].(string)
	require.True(t, strings.HasPrefix(file, "kb/architecture/scripts/host/"), file)

	q, _, isErr := callScript(t, tools, ctx, "query", map[string]any{"path": "kb/architecture"})
	require.False(t, isErr)
	require.Contains(t, q, "facts")

	ex, _, isErr := callScript(t, tools, ctx, "explain", map[string]any{"file": file})
	require.False(t, isErr)
	require.NotEmpty(t, ex)

	up, _, isErr := callScript(t, tools, ctx, "update", map[string]any{"file": file, "moment_name": "trigger:t1",
		"updates": map[string]any{"title": "Learned by a script, revised"}})
	require.False(t, isErr)
	require.Equal(t, file, up["file"])

	rt, _, isErr := callScript(t, tools, ctx, "retract", map[string]any{"file": file, "moment_name": "trigger:t1"})
	require.False(t, isErr)
	require.Equal(t, file, rt["file"])

	_, _, err := tools.Call(ctx, "hypothesize", nil)
	require.Error(t, err, "only the five tools exist")
}

// SameCodePathAsMCP: a validation failure, a bad src:// ref and a stale
// if_commit are refused exactly as the MCP tool refuses them, and nothing is
// committed; an `artifacts/<area>/` path is ACCEPTED here — the host's
// refusal is the host's, not the handler's (T19's second half) — and a
// `.knomit/` path is refused here as everywhere (F25). Sabotage: bypass
// ValidateFact in the tool set.
func TestScriptTools_SameCodePathAsMCP(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)
	tools := NewScriptTools(nil)
	ctx := scriptCtx(t, ri, store.Trailers{Trace: "t", Cause: strings.Repeat("c", 40), Trigger: "t1"})
	headBefore := headOf(t, ri, "agent/test")

	_, text, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "principles", "category": "mission/foo", "title": "Bad Principle",
			"body": "missing designer entity.", "kind": "pragmatic", "type": "policy", "confidence": 0.8, "sources": 1}},
	})
	require.True(t, isErr, "the ontology validation refuses it")
	require.Contains(t, text, "designer")

	_, text, isErr = callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "principles", "category": "mission/foo", "title": "Bad ref",
			"body": "b", "kind": "pragmatic", "type": "policy", "entities": []any{"designer"}, "confidence": 0.8, "sources": 1,
			"refs": []any{"src://nope"}}},
	})
	require.True(t, isErr, "the refs gate refuses a malformed src ref")
	require.Contains(t, strings.ToLower(text), "ref")
	require.Equal(t, headBefore, headOf(t, ri, "agent/test"), "a refused write commits nothing")

	learned, _, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts": []any{map[string]any{"topic": "principles", "category": "mission/foo", "title": "Good Principle",
			"body": "b", "kind": "pragmatic", "type": "policy", "entities": []any{"designer"}, "confidence": 0.8, "sources": 1}},
	})
	require.False(t, isErr)
	file := learned["commits"].([]any)[0].(map[string]any)["file"].(string)
	_, text, isErr = callScript(t, tools, ctx, "update", map[string]any{"file": file, "moment_name": "trigger:t1",
		"if_commit": strings.Repeat("0", 40), "updates": map[string]any{"title": "Stale"}})
	require.True(t, isErr, "a stale if_commit is refused")
	require.Contains(t, text, "current_commit")

	// The handler allows the artifacts root: the script host is what says no
	// to a script.
	priv, _, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts":       []any{map[string]any{"path": "artifacts/jobs/x.md", "title": "job state", "body": "b"}},
	})
	require.False(t, isErr, "the MCP tool accepts artifacts/<area>/ job state")
	require.Equal(t, "artifacts/jobs/x.md", priv["commits"].([]any)[0].(map[string]any)["file"])
	_, _, isErr = callScript(t, tools, ctx, "update", map[string]any{"file": "artifacts/jobs/x.md", "moment_name": "m", "updates": map[string]any{"body": "c"}})
	require.False(t, isErr)
	_, _, isErr = callScript(t, tools, ctx, "retract", map[string]any{"file": "artifacts/jobs/x.md", "moment_name": "m"})
	require.False(t, isErr)
	// .knomit/ is the system: the handler itself refuses it (F25).
	headBefore = headOf(t, ri, "agent/test")
	_, text, isErr = callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:t1",
		"facts":       []any{map[string]any{"path": ".knomit/jobs/x.md", "title": "job state", "body": "b"}},
	})
	require.True(t, isErr, "the MCP tool refuses .knomit/")
	require.Contains(t, text, "closed to the fact tools")
	require.Equal(t, headBefore, headOf(t, ri, "agent/test"))
}

// TrailersThroughUnchangedHandlers [T4, mcp variant]: the trailer set on the
// ctx reaches the commit the UNCHANGED learn/update/retract handlers write,
// byte-identical to the store's own stamping. Sabotage: rebuild the ctx in
// Call (drop the values).
func TestScriptTools_TrailersThroughUnchangedHandlers(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	tools := NewScriptTools(nil)
	cause := strings.Repeat("c", 40)
	ctx := scriptCtx(t, ri, store.Trailers{Trace: "story-1", Cause: cause, Trigger: "inbox"})
	want := "\n\nKnomit-Trace: story-1\nKnomit-Cause: " + cause + "\nKnomit-Trigger: inbox\n"

	learned, _, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:inbox",
		"facts": []any{map[string]any{"topic": "architecture", "category": "scripts/host", "title": "Stamped",
			"body": "body", "confidence": 0.8, "sources": 1}},
	})
	require.False(t, isErr)
	c := learned["commits"].([]any)[0].(map[string]any)
	file, hash := c["file"].(string), c["hash"].(string)
	require.Equal(t, "learn: trigger:inbox"+want, messageOf(t, ri, hash))

	up, _, isErr := callScript(t, tools, ctx, "update", map[string]any{"file": file, "moment_name": "trigger:inbox",
		"updates": map[string]any{"title": "Stamped, revised"}})
	require.False(t, isErr)
	require.True(t, strings.HasSuffix(messageOf(t, ri, up["commit"].(string)), want))

	rt, _, isErr := callScript(t, tools, ctx, "retract", map[string]any{"file": file, "moment_name": "trigger:inbox"})
	require.False(t, isErr)
	require.True(t, strings.HasSuffix(messageOf(t, ri, rt["commit"].(string)), want))

	// Without a set on the ctx the same handler stamps nothing.
	plain, _, isErr := callScript(t, tools, scriptCtx(t, ri, store.Trailers{}), "learn", map[string]any{
		"moment_name": "plain",
		"facts": []any{map[string]any{"topic": "architecture", "category": "scripts/host", "title": "Plain",
			"body": "body", "confidence": 0.8, "sources": 1}},
	})
	require.False(t, isErr)
	require.Equal(t, "learn: plain", messageOf(t, ri, plain["commits"].([]any)[0].(map[string]any)["hash"].(string)))
}

// T6 [#349]: knomit's set wins. On a script's (or recipe's) ctx, which carries
// knomit's own trace entries, a `trace` passed to update or retract — the
// host passes those options through unchanged — is REFUSED, even an empty one,
// and nothing is written; the same for learn reached with a trace. Sabotage:
// let WithAgentTrace overwrite (or merge into) the existing set.
// knomit_experiment applies the same rule (#349 extended to experiments). No
// script or recipe host exposes knomit_experiment today, so this calls the
// handler DIRECTLY under a ctx that carries knomit's own set — what a host
// would hand it — and checks the refusal comes before anything is opened.
// Sabotage: drop applyTrace from ExperimentHandler (red: the open runs).
func TestExperimentHandler_AgentTraceRefusedOnKnomitsSet(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := scriptCtx(t, ri, store.Trailers{Trace: "story-1", Cause: strings.Repeat("c", 40), Trigger: "inbox"})
	for _, trace := range []map[string]any{{"Knomit-Trace": "other-story"}, {}} {
		text, isErr := callHandler(t, ExperimentHandler(nil), ctx, map[string]any{"action": "open", "name": "from-a-host", "trace": trace})
		require.True(t, isErr, text)
		require.Contains(t, text, "already carries knomit's own trace entries")
		require.Empty(t, headOf(t, ri, "exp/from-a-host"), "nothing was opened")
	}
}

func TestScriptTools_AgentTraceRefusedOnKnomitsSet(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	tools := NewScriptTools(nil)
	ctx := scriptCtx(t, ri, store.Trailers{Trace: "story-1", Cause: strings.Repeat("c", 40), Trigger: "inbox"})
	learned, _, isErr := callScript(t, tools, ctx, "learn", map[string]any{
		"moment_name": "trigger:inbox",
		"facts": []any{map[string]any{"topic": "architecture", "category": "scripts/host", "title": "Stamped by knomit",
			"body": "body", "confidence": 0.8, "sources": 1}},
	})
	require.False(t, isErr)
	file := learned["commits"].([]any)[0].(map[string]any)["file"].(string)
	head := headOf(t, ri, "agent/test")

	agent := map[string]any{"Knomit-Trace": "other-story", "Ticket": "ABC-12"}
	for _, trace := range []map[string]any{agent, {}} {
		_, text, isErr := callScript(t, tools, ctx, "update", map[string]any{"file": file, "moment_name": "trigger:inbox",
			"updates": map[string]any{"title": "Overridden"}, "trace": trace})
		require.True(t, isErr, "update: %s", text)
		require.Contains(t, text, "already carries knomit's own trace entries")
		_, text, isErr = callScript(t, tools, ctx, "retract", map[string]any{"file": file, "moment_name": "trigger:inbox", "trace": trace})
		require.True(t, isErr, "retract: %s", text)
		require.Contains(t, text, "already carries knomit's own trace entries")
		_, text, isErr = callScript(t, tools, ctx, "learn", map[string]any{"moment_name": "trigger:inbox", "trace": trace,
			"facts": []any{map[string]any{"topic": "architecture", "category": "scripts/host", "title": "Another", "body": "b"}}})
		require.True(t, isErr, "learn: %s", text)
		require.Contains(t, text, "already carries knomit's own trace entries")
	}
	require.Equal(t, head, headOf(t, ri, "agent/test"), "nothing was written")
}
