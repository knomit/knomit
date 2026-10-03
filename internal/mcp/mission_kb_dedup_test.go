package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// The mission's knowledge base (examples/mission-kb/): the topics two agents
// write near-identical facts into by design must be learn_dedup: off, or
// knomit_learn merges one agent's fact into another's.
//
// That merge is a SHARED-FACT WRITE in disguise: the second writer's
// experiment then edits the first writer's file, and two parallel tasks
// doing it conflict at their experiment commits, which is what stopped the
// first mission's parallel cross-checks. For a counter-hypothesis it is
// worse: the counter is swallowed by what it counters.
//
// The length embedder puts equal-length text at cosine 1.0, so the dedup
// path is taken deterministically; the control (the same ontology with the
// topic's `learn_dedup: off` removed) proves the shipped case is not vacuous.

func missionKBOntology(t *testing.T, topic string, dedupOff bool) *fact.Ontology {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission-kb", ".knomit", "ontology.yaml"))
	require.NoError(t, err)
	src := string(raw)
	if !dedupOff {
		i := strings.Index(src, "\n  "+topic+":\n")
		require.GreaterOrEqual(t, i, 0, "topic %s in the shipped knowledge-base ontology", topic)
		const attr = "    attributes:\n      learn_dedup: off\n"
		j := strings.Index(src[i:], attr)
		require.GreaterOrEqual(t, j, 0, "topic %s's learn_dedup: off", topic)
		src = src[:i+j] + src[i+j+len(attr):]
	}
	o, err := fact.ParseOntology([]byte(src))
	require.NoError(t, err)
	require.Equal(t, dedupOff, o.LearnDedupOff(topic), "the fixture is what it says")
	return o
}

func learnOne(f map[string]any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "kb", "facts": []any{f}}
	return req
}

// SABOTAGE: delete `learn_dedup: off` under forecast in the shipped
// knowledge-base ontology → the shipped case folds the second hypothesis
// into the first → red.
func TestMissionKB_ForecastNeverMerges(t *testing.T) {
	// Two writers' hypotheses on one subject and granularity: equal length,
	// one entity, different words. The second could be a counter.
	h := func(who string) map[string]any {
		return map[string]any{
			"topic": "forecast", "category": "subject-a/month", "type": "hypothesis",
			"title": "Subject A happens in 2026-10 (" + who + ")", "confidence": 0.4,
			"body": "predicted: 2026-10\nsettles_true_if: the public record shows it in the month\n" +
				"settles_false_if: the public record lacks it at month end\n\nReasoning by " + who + ".",
			"entities": []any{"subject-a"}, "expires": "2026-10-31T23:59:59Z",
		}
	}
	for _, c := range []struct {
		name     string
		dedupOff bool
	}{{"shipped", true}, {"without learn_dedup off", false}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, emb, _ := newMissionDedupRepo(t, missionKBOntology(t, "forecast", c.dedupOff))
			r1, err := LearnHandler(emb)(ctx, learnOne(h("agent-1")))
			require.NoError(t, err)
			require.False(t, r1.IsError, resultText(t, r1))
			first := mergedFactPath(t, r1)
			r2, err := LearnHandler(emb)(ctx, learnOne(h("agent-2")))
			require.NoError(t, err)
			if !c.dedupOff {
				// Control: the second writer's hypothesis is folded into the
				// first writer's file (the write lands on ANOTHER agent's
				// hypothesis), so it is never its own fact.
				require.False(t, r2.IsError, resultText(t, r2))
				require.Equal(t, first, mergedFactPath(t, r2), "control: folded into the first writer's hypothesis")
				return
			}
			require.False(t, r2.IsError, resultText(t, r2))
			require.NotEqual(t, first, mergedFactPath(t, r2), "two writers, two hypotheses, two files")
		})
	}
}

// Two cross-checkers' verdicts on ONE target: near-identical by design
// (templated lines, one target, one entity). They must land as two facts, so
// that no cross-check ever writes to a file another one wrote and the fold
// sees every verdict.
//
// SABOTAGE: delete `learn_dedup: off` under verdicts in the shipped
// knowledge-base ontology → the second verdict folds into the first → red.
func TestMissionKB_VerdictsNeverMerge(t *testing.T) {
	v := func(task, verdict string) map[string]any {
		return map[string]any{
			"topic": "verdicts", "category": "xcheck", "type": "observation",
			"title": "Verdict of " + task, "confidence": 0.7,
			"body": "verdict: " + verdict + "\ntarget: kb/forecast/subject-a/month/aaaa.md\n" +
				"suggested_confidence: 0.55\n\nReasons of " + task + ".",
			"entities": []any{"subject-a"}, "refs": []any{"https://example.org/target/kb/forecast/subject-a/month/aaaa.md"},
		}
	}
	for _, c := range []struct {
		name     string
		dedupOff bool
	}{{"shipped", true}, {"without learn_dedup off", false}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, emb, _ := newMissionDedupRepo(t, missionKBOntology(t, "verdicts", c.dedupOff))
			// One category for both, the worst case: the dedup search scope is
			// the category directory, so per-task categories alone would hide
			// the merge. Same-length text puts them at cosine 1.0.
			r1, err := LearnHandler(emb)(ctx, learnOne(v("xcheck-a", "corroborate")))
			require.NoError(t, err)
			require.False(t, r1.IsError, resultText(t, r1))
			first := mergedFactPath(t, r1)
			r2, err := LearnHandler(emb)(ctx, learnOne(v("xcheck-b", "contradict ")))
			require.NoError(t, err)
			require.False(t, r2.IsError, resultText(t, r2))
			if !c.dedupOff {
				require.Equal(t, first, mergedFactPath(t, r2), "control: the second verdict folds into the first")
				return
			}
			require.NotEqual(t, first, mergedFactPath(t, r2), "two verdicts, two files")
		})
	}
}
