package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// F26: the mission repo's one topic of facts ABOUT the target knowledge
// base's facts, annotations/<task id>/, typed by F22 `context` declared in
// the SHIPPED mission ontology (examples/mission/.knomit/ontology.yaml). A
// cross-check writes one annotation per fact it checks, with
// context.kind: verdict and a kb:// ref to the fact; the fold reads them back
// by path plus context.

// annotation is a cross-check's verdict as the work-task skill's call writes
// it: one target, one entity, templated text, so two of them on one target
// are near-identical by design.
func annotation(task, title string, ctx map[string]any) map[string]any {
	f := map[string]any{
		"topic": "annotations", "category": task, "type": "observation",
		"title": title, "body": "The evidence does not support the claim.", "confidence": 0.8,
		"entities": []any{"subject-a"},
		"refs":     []any{"kb://bc6eac5f37df/kb/hypotheses/agents/memory/1a2b3c4d.md", "https://example.org/evidence"},
	}
	if ctx != nil {
		f["context"] = ctx
	}
	return f
}

// missionAnnotationsRepo is a repo whose ontology is the SHIPPED mission
// ontology, parsed as a new one (so a bad declaration is fatal here).
func missionAnnotationsRepo(t *testing.T) context.Context {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", ".knomit", "ontology.yaml"))
	require.NoError(t, err)
	_, _, ctx, _ := newContextRepo(t, string(raw))
	return ctx
}

func verdictCtx(verdict string) map[string]any {
	return map[string]any{"kind": "verdict", "verdict": verdict, "confidence": 0.4}
}

func learnAnnotation(t *testing.T, ctx context.Context, f map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "xcheck", "facts": []any{f}}
	res, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	return res
}

// TestMissionAnnotations_TypedByContext: on the shipped ontology a verdict
// with the declared keys lands, and each way of breaking the declaration is
// refused with an error that names the key.
//
// SABOTAGE: drop `required: true` from kind → the annotation without kind
// lands → red; add `maybe` to verdict's values → red; delete the
// annotations topic's `context:` block → the good verdict is refused → red.
func TestMissionAnnotations_TypedByContext(t *testing.T) {
	ctx := missionAnnotationsRepo(t)

	res := learnAnnotation(t, ctx, annotation("xcheck-1", "Verdict: contradicted", verdictCtx("contradict")))
	require.False(t, res.IsError, "a verdict with the declared keys lands: %s", resultText(t, res))
	res = learnAnnotation(t, ctx, annotation("xcheck-1", "Kind only", map[string]any{"kind": "verdict"}))
	require.False(t, res.IsError, "only kind is required: %s", resultText(t, res))

	for _, c := range []struct {
		name string
		ctx  map[string]any
		key  string
	}{
		{"no kind", map[string]any{"verdict": "contradict", "confidence": 0.4}, `context key "kind"`},
		{"no context at all", nil, `context key "kind"`},
		{"an undeclared key", map[string]any{"kind": "verdict", "verdict": "contradict", "target": "kb/x.md"}, `context key "target"`},
		{"verdict: maybe", map[string]any{"kind": "verdict", "verdict": "maybe"}, `context key "verdict"`},
		{"a kind not declared", map[string]any{"kind": "note"}, `context key "kind"`},
		{"confidence above 1", map[string]any{"kind": "verdict", "confidence": 1.5}, `context key "confidence"`},
	} {
		res := learnAnnotation(t, ctx, annotation("xcheck-1", "Refused: "+c.name, c.ctx))
		require.True(t, res.IsError, "%s must be refused", c.name)
		require.Contains(t, resultText(t, res), c.key, "%s: the error names the key", c.name)
	}
}

// TestMissionAnnotations_TwoTasksTwoPaths: two cross-checks' verdicts on ONE
// target, near-identical by design (same target, same entity, equal-length
// text), land at two paths, one in each task's folder.
//
// The control is not vacuous: learn's dedup search scope is a RAW path prefix
// (learn.go, #260), so a verdict learned into annotations/xcheck-1/ searches
// annotations/xcheck-12/ too. With `learn_dedup: off` removed from the topic,
// the second task's verdict folds into the first task's file.
//
// SABOTAGE: delete `learn_dedup: off` under annotations in the shipped
// ontology → the shipped case folds → red.
func TestMissionAnnotations_TwoTasksTwoPaths(t *testing.T) {
	for _, c := range []struct {
		name     string
		dedupOff bool
	}{{"shipped", true}, {"without learn_dedup off", false}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, emb, _ := newMissionDedupRepo(t, missionOntology(t, "annotations", c.dedupOff))
			learn := func(f map[string]any) *mcpgo.CallToolResult {
				t.Helper()
				var req mcpgo.CallToolRequest
				req.Params.Arguments = map[string]any{"moment_name": "xcheck", "facts": []any{f}}
				res, err := LearnHandler(emb)(ctx, req)
				require.NoError(t, err)
				return res
			}
			r1 := learn(annotation("xcheck-12", "Verdict of xcheck-12", verdictCtx("corroborate")))
			require.False(t, r1.IsError, resultText(t, r1))
			first := mergedFactPath(t, r1)
			require.Contains(t, first, "/annotations/xcheck-12/")
			r2 := learn(annotation("xcheck-1", "Verdict of xcheck-1x", verdictCtx("contradict")))
			if !c.dedupOff {
				// Control: the second task's verdict does not get a file of
				// its own: it is folded into the first task's file.
				require.False(t, r2.IsError, resultText(t, r2))
				require.Equal(t, first, mergedFactPath(t, r2), "control: the second verdict folds into the first task's file")
				return
			}
			require.False(t, r2.IsError, resultText(t, r2))
			second := mergedFactPath(t, r2)
			require.NotEqual(t, first, second, "two tasks, two verdicts, two files")
			require.Contains(t, second, "/annotations/xcheck-1/")
		})
	}
}

// TestMissionAnnotations_QueryByContext: the fold's read, as the work-task
// skill spells it — path kb/annotations/<task>/ with context {kind: verdict}
// — returns exactly that task's verdicts: not a sibling task's whose id it
// prefixes, not another task's. Without the trailing slash the prefix reaches
// the sibling, which is why the skill and README keep it.
//
// SABOTAGE: drop `Context: ctxFilter` from parseQueryFilters (the F22 filter
// this read relies on) → the verdict: contradict read returns both → red.
func TestMissionAnnotations_QueryByContext(t *testing.T) {
	ctx := missionAnnotationsRepo(t)
	for _, a := range []struct{ task, title, verdict string }{
		{"xcheck-1", "A1 verdict", "corroborate"},
		{"xcheck-1", "A2 verdict", "contradict"},
		{"xcheck-12", "B1 verdict", "contradict"},
		{"xcheck-2", "C1 verdict", "contradict"},
	} {
		res := learnAnnotation(t, ctx, annotation(a.task, a.title, verdictCtx(a.verdict)))
		require.False(t, res.IsError, resultText(t, res))
	}
	titles := func(args map[string]any) []string {
		t.Helper()
		resp, res := queryFacts(t, ctx, args)
		require.False(t, res.IsError, resultText(t, res))
		out := titlesOf(resp)
		sort.Strings(out)
		return out
	}
	require.Equal(t, []string{"A1 verdict", "A2 verdict"},
		titles(map[string]any{"path": "kb/annotations/xcheck-1/", "context": map[string]any{"kind": "verdict"}}),
		"exactly that task's verdicts")
	require.Equal(t, []string{"A2 verdict"},
		titles(map[string]any{"path": "kb/annotations/xcheck-1/", "context": map[string]any{"kind": "verdict", "verdict": "contradict"}}))
	require.Equal(t, []string{"A1 verdict", "A2 verdict", "B1 verdict", "C1 verdict"},
		titles(map[string]any{"path": "kb/annotations/", "context": map[string]any{"kind": "verdict"}}),
		"every verdict of the mission")
	require.Equal(t, []string{"A1 verdict", "A2 verdict", "B1 verdict"},
		titles(map[string]any{"path": "kb/annotations/xcheck-1", "context": map[string]any{"kind": "verdict"}}),
		"fixture: without the trailing slash the path prefix reaches xcheck-12")
}
