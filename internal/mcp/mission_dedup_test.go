package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// F08 PR D, T-D4: the mission template's signal topics need learn_dedup: off.
//
// Two tasks in one lane, or two working copies in one agent's queue, are
// templated text that differs by an id: near-identical by design. With dedup
// on, the second is merged into the first (the dedup search scope is the
// incoming fact's category directory). Measured: a second TASK is refused
// outright (the merge would carry two entities and the one-id rule refuses
// it), so it cannot be posted at all; a second WORKING COPY is folded into
// the first, which then names both tasks. The length embedder puts
// equal-length text at cosine 1.0, so the dedup path is taken
// deterministically.
//
// Claims are NOT the case the RCA named (P6d): the template files each
// claimer's claim under its own category (claims/<task>/<agent-id>/), and the
// search never reaches a sibling category, so two claimers' claims cannot
// fold even with dedup on. The flag still stands on claims, for a re-claim in
// the same category.
//
// The SHIPPED ontology keeps both facts, each with its own expires; the same
// ontology with learn_dedup removed from the topic folds them, which is the
// control that proves the first half is not vacuous.

func missionOntology(t *testing.T, topic string, dedupOff bool) *fact.Ontology {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "mission", ".knomit", "ontology.yaml"))
	require.NoError(t, err)
	src := string(raw)
	if !dedupOff {
		// The topic's own attributes block, the first one after its key.
		i := strings.Index(src, "\n  "+topic+":\n")
		require.GreaterOrEqual(t, i, 0, "topic %s in the shipped ontology", topic)
		const attr = "    attributes:\n      learn_dedup: off\n"
		j := strings.Index(src[i:], attr)
		require.GreaterOrEqual(t, j, 0)
		src = src[:i+j] + src[i+j+len(attr):]
	}
	o, err := fact.ParseOntology([]byte(src))
	require.NoError(t, err)
	if !dedupOff {
		require.False(t, o.LearnDedupOff(topic), "the control really has dedup on")
	}
	// The shipped file is taken as it is: whether its flag holds is what the
	// learns below measure.
	return o
}

func newMissionDedupRepo(t *testing.T, o *fact.Ontology) (context.Context, store.BatchEmbedder, *store.Service) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "mission", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: o, OntologyRoot: "kb", Embedder: emb,
	})
	return repos.WithRepoInstance(context.Background(), ri), emb, svc
}

// signalReq is one template-shaped signal: title and body differ only by the
// task id, as a poster's templated messages and the scripts' do.
func signalReq(topic, category, task string, expires time.Time) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "post",
		"facts": []any{map[string]any{
			"topic": topic, "category": category, "kind": "pragmatic", "type": "signal",
			"title": "Task " + task, "body": "Work item " + task + ": see the charter.",
			"entities": []any{task}, "expires": expires.UTC().Format(time.RFC3339), "confidence": 1, "sources": 1,
		}},
	}
	return req
}

// SABOTAGE: delete `learn_dedup: off` under tasks in the shipped ontology →
// the tasks/shipped subtest folds the second task → red.
func TestMission_SignalTopicsNeedDedupOff(t *testing.T) {
	e1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e2 := e1.Add(30 * time.Second)
	for _, where := range []struct{ topic, category string }{
		{"tasks", "lane-a"},
		{"inbox", "peer-bbbbbbbb/working"},
	} {
		for _, c := range []struct {
			name     string
			dedupOff bool
		}{{"shipped", true}, {"without learn_dedup off", false}} {
			t.Run(where.topic+"/"+c.name, func(t *testing.T) {
				ctx, emb, svc := newMissionDedupRepo(t, missionOntology(t, where.topic, c.dedupOff))
				r1, err := LearnHandler(emb)(ctx, signalReq(where.topic, where.category, "task-1", e1))
				require.NoError(t, err)
				require.False(t, r1.IsError, resultText(t, r1))
				first := mergedFactPath(t, r1)
				r2, err := LearnHandler(emb)(ctx, signalReq(where.topic, where.category, "task-2", e2))
				require.NoError(t, err)
				if !c.dedupOff {
					// Control: with dedup on, the second signal is never its own
					// fact. Where a rule forbids the merged shape (a task carries
					// ONE entity) the post is refused outright; elsewhere it is
					// folded into the first, which then names both tasks.
					if r2.IsError {
						require.Contains(t, resultText(t, r2), "dedup-merge")
						return
					}
					require.Equal(t, first, mergedFactPath(t, r2), "control: the second signal folds into the first")
					got, err := svc.Facts().ReadFact(context.Background(), "agent/test", first, nil)
					require.NoError(t, err)
					f, err := fact.ParseFact(first, got.Content)
					require.NoError(t, err)
					require.ElementsMatch(t, []string{"task-1", "task-2"}, f.Entities, "one working copy now names two tasks")
					return
				}
				require.False(t, r2.IsError, resultText(t, r2))
				second := mergedFactPath(t, r2)
				require.NotEqual(t, first, second, "two signals, two files")
				for p, want := range map[string]time.Time{first: e1, second: e2} {
					got, err := svc.Facts().ReadFact(context.Background(), "agent/test", p, nil)
					require.NoError(t, err)
					f, err := fact.ParseFact(p, got.Content)
					require.NoError(t, err)
					require.Equal(t, want.Format(time.RFC3339), f.Expires, "%s keeps its own timer", p)
				}
			})
		}
	}
}
