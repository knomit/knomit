package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// F22: merges and work items.

const synthContextOntology = `id: t
name: T
topics:
  verdicts:
    description: verdicts
    context:
      task:    {type: string}
      verdict: {type: enum, values: [agree, disagree, unsure]}
  technology:
    description: no context declared
`

func synthOntology(t *testing.T) *fact.Ontology {
	t.Helper()
	o, err := fact.ParseNewOntology([]byte(synthContextOntology))
	require.NoError(t, err)
	return o
}

func seedWithContext(t *testing.T, svc *store.Service, branch, path string, conf float64, ctx map[string]any) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title, f.Body, f.Type = path, "body of "+path, fact.Observation
	f.Domain, f.Entities, f.Refs = []string{"test"}, []string{}, []string{}
	f.Confidence, f.Sources = conf, 1
	f.Context = ctx
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

func contextWarn(warns []string) string {
	for _, w := range warns {
		if strings.Contains(w, "context keys not carried") {
			return w
		}
	}
	return ""
}

// Prune merge: three members agree on task and disagree on verdict → the new
// fact carries task only, and a warn names verdict (and not task).
//
// SABOTAGE: take the union instead of the agreement → verdict lands → red.
func TestApplyPruneDecisions_ContextKeepsOnlyAgreedKeys(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	seedWithContext(t, svc, branch, "kb/verdicts/a.md", 0.8, map[string]any{"task": "t-17", "verdict": "agree"})
	seedWithContext(t, svc, branch, "kb/verdicts/b.md", 0.8, map[string]any{"task": "t-17", "verdict": "disagree"})
	seedWithContext(t, svc, branch, "kb/verdicts/c.md", 0.8, map[string]any{"task": "t-17", "verdict": "agree"})
	sink, warns := collectWarns()
	stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, []MergeEntry{{
		Paths:  []string{"kb/verdicts/a.md", "kb/verdicts/b.md", "kb/verdicts/c.md"},
		Merged: mergedFact{Path: "kb/verdicts/m.md", Title: "Merged", Body: "merged body", Type: "observation"},
	}}, "review-test", sink, branch, bareRefFixture, "kb", synthOntology(t))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged, "fixture: the merge must have happened")

	merged := readFactForTest(t, svc, branch, mergedFactPath(t, svc, branch, "Merged"))
	require.Equal(t, map[string]any{"task": "t-17"}, merged.Context)
	w := contextWarn(*warns)
	require.NotEmpty(t, w, "the dropped key must be named: %v", *warns)
	require.Contains(t, w, ": verdict")
	require.NotContains(t, w, "task")
}

// An agreed key the merged fact's OWN topic does not declare is dropped too —
// a prune merge never writes an undeclared key — and with no ontology nothing
// is carried at all.
func TestApplyPruneDecisions_ContextAgreedButUndeclaredAtTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		o      *fact.Ontology
	}{
		{"undeclared at the target topic", "kb/technology/m.md", synthOntology(t)},
		{"no ontology", "kb/verdicts/m.md", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, branch := newSourcesTestRepo(t)
			seedWithContext(t, svc, branch, "kb/verdicts/a.md", 0.8, map[string]any{"task": "t-17"})
			seedWithContext(t, svc, branch, "kb/verdicts/b.md", 0.8, map[string]any{"task": "t-17"})
			sink, warns := collectWarns()
			stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, []MergeEntry{{
				Paths:  []string{"kb/verdicts/a.md", "kb/verdicts/b.md"},
				Merged: mergedFact{Path: tc.target, Title: "Merged", Body: "merged body", Type: "observation"},
			}}, "review-test", sink, branch, bareRefFixture, "kb", tc.o)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Merged)
			merged := readFactForTest(t, svc, branch, mergedFactPath(t, svc, branch, "Merged"))
			require.Nil(t, merged.Context)
			require.Contains(t, contextWarn(*warns), ": task")
		})
	}
}

// C3: review's confidence-only rewrite (a prune "update") re-serializes the
// fact it parsed, and keeps its context.
func TestApplyPruneDecisions_ConfidenceUpdateKeepsContext(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	seedWithContext(t, svc, branch, "kb/verdicts/a.md", 0.8, map[string]any{"task": "t-17", "verdict": "agree"})
	stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(),
		[]PruneDecision{{Path: "kb/verdicts/a.md", Action: "update", Confidence: 0.42}}, nil,
		"review-test", func(ProgressEvent) {}, branch, bareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.Contains(t, stats.Rewritten, "kb/verdicts/a.md", "fixture: the update must have been applied")
	got := readFactForTest(t, svc, branch, "kb/verdicts/a.md")
	require.Equal(t, 0.42, got.Confidence)
	require.Equal(t, map[string]any{"task": "t-17", "verdict": "agree"}, got.Context)
}

// A fact whose context arrived malformed via git is not rewritten by a review
// confidence change (the rewrite would delete the map): skipped, with a warn.
func TestApplyPruneDecisions_ConfidenceUpdateSkipsMalformedContext(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	raw := "---\ntype: observation\ndomain: [test]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\ncontext: {task: [t-17]}\n---\n# M\n\nbody\n"
	_, err := svc.Facts().WriteFact(context.Background(), branch, "kb/verdicts/m.md", raw, "sync", "")
	require.NoError(t, err)
	sink, warns := collectWarns()
	stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(),
		[]PruneDecision{{Path: "kb/verdicts/m.md", Action: "update", Confidence: 0.42}}, nil,
		"review-test", sink, branch, bareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.Equal(t, 0, stats.Updated)
	require.Contains(t, strings.Join(*warns, "\n"), "update kb/verdicts/m.md skipped: its context is malformed")
	stored, err := svc.Facts().ReadFact(context.Background(), branch, "kb/verdicts/m.md", nil)
	require.NoError(t, err)
	require.Equal(t, raw, stored.Content, "untouched")
}

// Review's pairwise dedup keeps the WINNER's map whole; none of the loser's
// keys appear.
func TestDedupCluster_ContextIsTheWinnersWholeMap(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)
	const winnerPath, loserPath = "kb/technology/winner.md", "kb/technology/loser.md"
	seedWithContext(t, svc, branch, winnerPath, 0.9, map[string]any{"task": "t-1"})
	seedWithContext(t, svc, branch, loserPath, 0.5, map[string]any{"task": "t-2", "verdict": "agree"})
	cluster := []factForLLM{
		{File: winnerPath, Kind: "epistemic", Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: 0.9, Sources: 1},
		{File: loserPath, Kind: "epistemic", Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: 0.5, Sources: 1},
	}
	idx := &fixedPairSearch{SearchQuery: svc.Search(), results: []store.SearchResult{searchHit(winnerPath), searchHit(loserPath)}}
	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), idx, 0.92, "test", func(ProgressEvent) {}, branch, bareRefFixture)
	require.NoError(t, err)
	require.Len(t, surviving, 1)
	require.Equal(t, winnerPath, surviving[0].File, "fixture: the more confident fact wins")
	require.Equal(t, map[string]any{"task": "t-1"}, readFactForTest(t, svc, branch, winnerPath).Context)
}

// Distill writes a NEW claim; it inherits nothing from its inputs.
func TestApplyDistillDecisions_NewClaimInheritsNoContext(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	seedWithContext(t, svc, branch, "kb/verdicts/a.md", 0.8, map[string]any{"task": "t-17"})
	seedWithContext(t, svc, branch, "kb/verdicts/b.md", 0.8, map[string]any{"task": "t-17"})
	df := distillFact{
		Path: "kb/verdicts/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"verdicts"}, Confidence: 0.9, Refs: []string{"kb/verdicts/a.md", "kb/verdicts/b.md"},
	}
	_, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.Search(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1)
	require.Nil(t, readFactForTest(t, svc, branch, written[0].Path).Context)
}

// C1 (a)(b): a prune/distill member and a hypothesize seed carry their context
// as a JSON object INSIDE the fact data the judge reads.
func TestWorkItems_CarryContextAsFactData(t *testing.T) {
	f := fact.NewFact("kb/verdicts/a.md")
	f.Title, f.Body, f.Type, f.Confidence, f.Sources = "V", "body", fact.Observation, 0.8, 1
	f.Context = map[string]any{"task": "t-17", "verdict": "disagree"}

	members, err := json.Marshal(factsForLLM([]fact.Fact{f}, bareRefFixture))
	require.NoError(t, err)
	require.Contains(t, string(members), `"context":{"task":"t-17","verdict":"disagree"}`, "prune/distill member")

	seed, err := json.Marshal(f) // hypothesizeStrategy.Plan marshals the fact itself
	require.NoError(t, err)
	require.Contains(t, string(seed), `"context":{"task":"t-17","verdict":"disagree"}`, "hypothesize seed")

	plain := f
	plain.Context = nil
	none, err := json.Marshal(factsForLLM([]fact.Fact{plain}, bareRefFixture))
	require.NoError(t, err)
	require.NotContains(t, string(none), "context", "omitted when empty: context-free payloads are byte-identical")
}

// C1 (d): the pager measures each fact INCLUDING its context, so a page of
// maximum-context facts still fits the delivered cap; and context does change
// the packing (it is counted, not ignored).
func TestFactPages_CountContext(t *testing.T) {
	maxCtx := map[string]any{}
	for i := 0; i < fact.MaxContextKeys; i++ {
		maxCtx[fmt.Sprintf("key_%02d", i)] = strings.Repeat("v", fact.MaxContextValueBytes)
	}
	var with, without []factForLLM
	for i := 0; i < 12; i++ {
		f := factForLLM{File: fmt.Sprintf("kb/v/%02d.md", i), Title: "T", Body: "body", Type: "observation", Kind: "epistemic", Confidence: 0.8, Sources: 1}
		without = append(without, f)
		f.Context = maxCtx
		with = append(with, f)
	}
	const budget = 20000
	pages := packFactPages(with, budget)
	total := 0
	for _, p := range pages {
		total += len(p)
		b, err := json.Marshal(p)
		require.NoError(t, err)
		if len(p) > 1 {
			require.LessOrEqual(t, deliveredFactsLen(b), budget, "a multi-fact page fits the cap with context counted")
		}
	}
	require.Equal(t, len(with), total, "no fact lost")
	require.Greater(t, len(pages), len(packFactPages(without, budget)), "context bytes are counted")
}

// C1 (c): no prompt TEMPLATE and no Go prompt builder interpolates a fact's
// context outside the JSON fact data. Templates: no action names it. Go: no
// fmt/strings.Builder call in the prompt-building files takes a `.Context`
// selector as an argument.
func TestPrompts_NeverInterpolateContext(t *testing.T) {
	action := regexp.MustCompile(`\{\{[^}]*\}\}`)
	files, err := filepath.Glob(filepath.Join("prompts", "large", "*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "fixture: the templates must be found")
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, a := range action.FindAllString(string(b), -1) {
			require.NotContains(t, strings.ToLower(a), "context", "%s: template action %s", f, a)
		}
	}

	builders := []string{"prompts.go", "prompt_review.go", "review_strategy.go", "hypothesize_strategy.go", "discovery.go"}
	for _, name := range builders {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Sprintf", "Fprintf", "Printf", "WriteString", "Execute", "ExecuteTemplate":
			default:
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					if s, ok := m.(*ast.SelectorExpr); ok && s.Sel.Name == "Context" {
						t.Errorf("%s: %s passes a .Context selector into prompt text", fset.Position(s.Pos()), sel.Sel.Name)
					}
					return true
				})
			}
			return true
		})
	}
}
