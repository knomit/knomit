package synthesize_test

// Scenario worlds load through knomit's own write path: the REAL
// knomit_learn / knomit_update / knomit_retract handlers via
// mcp.NewScriptTools. This file is in package synthesize_test because
// internal/mcp imports internal/synthesize.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

const exampleSpec = "testdata/scenario/example/spec.yaml"

func loadSpec(t *testing.T, s *synthesize.Scenario, emb store.BatchEmbedder) (*synthesize.LoadedWorld, error) {
	t.Helper()
	w, err := synthesize.LoadScenario(context.Background(), s, synthesize.ScenarioLoadOptions{
		Dir: t.TempDir(), Tools: mcp.NewScriptTools(emb), Embedder: emb,
	})
	if w != nil {
		t.Cleanup(func() { _ = w.Close() })
	}
	return w, err
}

func readAt(t *testing.T, w *synthesize.LoadedWorld, id, commit string) fact.Fact {
	t.Helper()
	res, err := w.Svc.Facts().ReadFact(context.Background(), w.Branch, w.Paths[id], &store.ReadFactOpts{AtCommit: commit})
	require.NoError(t, err)
	f, err := fact.ParseFact(w.Paths[id], res.Content)
	require.NoError(t, err)
	return f
}

// TestScenarioExample_LoadsThroughWritePath: the example world's facts,
// history and refs come back from the store exactly as the spec says.
// Sabotage: drop the update step's refs resolution (refs keep "fact:f05") or
// skip the retract step; the ref and liveness assertions fail.
func TestScenarioExample_LoadsThroughWritePath(t *testing.T) {
	s, err := synthesize.ReadScenarioFile(exampleSpec)
	require.NoError(t, err)
	w, err := loadSpec(t, s, nil)
	require.NoError(t, err)
	ctx := context.Background()

	require.Len(t, w.Paths, 10, "every spec fact got its own server-minted path")
	require.Len(t, w.StepCommits, 6, "one commit per history step")
	for id, p := range w.Paths {
		f := s.FactByID(id)
		require.True(t, strings.HasPrefix(p, "kb/"+f.Topic+"/"+f.Category+"/"), "%s at %s", id, p)
	}

	// The ontology is the world's own, on the consensus branch at init.
	sys, err := w.Svc.Facts().ReadFact(ctx, w.Branch, ".knomit/ontology.yaml", nil)
	require.NoError(t, err)
	require.Contains(t, sys.Content, "id: orchard-example")

	// f03 was learned, then rewritten in place: two revisions, both real.
	require.Len(t, w.Revisions["f03"], 2)
	v1 := readAt(t, w, "f03", w.Revisions["f03"][0])
	v2 := readAt(t, w, "f03", w.Revisions["f03"][1])
	require.Equal(t, "Velmora pears keep eleven weeks in the Hollin cellar because the cellar is cold", v1.Title)
	require.True(t, strings.HasSuffix(v2.Title, "because its air is humid, not because it is cold"), v2.Title)
	require.InDelta(t, 0.85, v2.Confidence, 1e-9)
	// Refs were resolved to the minted paths and canonicalised by the server.
	local := func(f fact.Fact) []string {
		var out []string
		for _, r := range f.Refs {
			c := fact.ClassifyRef(r, w.RepoID)
			require.Equal(t, fact.RefLocalFact, c.Kind, r)
			out = append(out, w.IDOfPath(c.Path))
		}
		return out
	}
	require.Equal(t, []string{"f01", "f02"}, local(v1))
	require.Equal(t, []string{"f01", "f02", "f05"}, local(v2))
	hist, _, err := w.Svc.HistoryQuery().PathHistory(ctx, w.Branch, w.Paths["f03"], w.Head, nil, 10)
	require.NoError(t, err)
	require.Len(t, hist, 2, "knomit's own history index sees the supersession")
	require.Equal(t, "modified", hist[0].Action)
	require.Equal(t, w.Revisions["f03"][1], hist[0].Commit)

	// Kinds and types landed as written.
	f06 := readAt(t, w, "f06", w.Head)
	require.Equal(t, fact.Pragmatic, f06.Kind)
	require.Equal(t, fact.Type("policy"), f06.Type)
	f07 := readAt(t, w, "f07", w.Head)
	require.Equal(t, fact.Type("hypothesis"), f07.Type)
	require.Equal(t, []string{"Velmora pear", "Brask shed"}, f07.Entities)

	// f10 was retracted: live before its retraction, gone at the head.
	live, err := w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, w.Paths["f10"], w.Head)
	require.NoError(t, err)
	require.False(t, live)
	live, err = w.Svc.Search().FactLiveAtCommit(ctx, w.Branch, w.Paths["f10"], w.StepCommits[4])
	require.NoError(t, err)
	require.True(t, live)
	require.Equal(t, w.Head, w.RetractedAt["f10"])
	require.NoError(t, synthesize.CheckGold(ctx, w))
}

// TestCheckGold_RefusesRetractedSupport: gold that rests on a fact the
// history retracts is refused. Sabotage: drop the liveness loop.
func TestCheckGold_RefusesRetractedSupport(t *testing.T) {
	b, err := os.ReadFile(exampleSpec)
	require.NoError(t, err)
	mut := strings.Replace(string(b), "support: [f03, f01, f02, f05]\n    forbidden: [f04, f07, f10]", "support: [f03, f01, f02, f05, f10]\n    forbidden: [f04, f07]", 1)
	require.NotEqual(t, string(b), mut)
	s, err := synthesize.ParseScenario([]byte(mut))
	require.NoError(t, err)
	w, err := loadSpec(t, s, nil)
	require.NoError(t, err)
	err = synthesize.CheckGold(context.Background(), w)
	require.ErrorContains(t, err, "fact f10 must be cited but is retracted at the head")
}

// TestScenarioLoad_RefusesOntologyRule: a fact that breaks one of the
// world's ontology rules is refused by knomit_learn, and the error names the
// fact and the rule. Sabotage: have the loader write files directly instead
// of calling the handler; nothing refuses.
func TestScenarioLoad_RefusesOntologyRule(t *testing.T) {
	b, err := os.ReadFile(exampleSpec)
	require.NoError(t, err)
	// f06 is a rule; make it an epistemic observation, which rule-is-policy refuses.
	broken := strings.Replace(string(b), "    kind: pragmatic\n    type: policy\n", "    type: observation\n", 1)
	require.NotEqual(t, string(b), broken)
	s, err := synthesize.ParseScenario([]byte(broken))
	require.NoError(t, err, "the static check passes; only the write path knows the rule")
	_, err = loadSpec(t, s, nil)
	require.Error(t, err)
	var se *synthesize.ScenarioError
	require.ErrorAs(t, err, &se)
	require.Contains(t, se.Where, "fact f06")
	require.Contains(t, err.Error(), "rule-is-policy")
	require.Contains(t, err.Error(), "knomit_learn refused")
}

// TestScenarioCheck_RejectsBadSpecs: the static half names what is wrong.
func TestScenarioCheck_RejectsBadSpecs(t *testing.T) {
	b, err := os.ReadFile(exampleSpec)
	require.NoError(t, err)
	base := string(b)
	cases := []struct{ name, from, to, want string }{
		{"unknown key", "ontology_shape: general", "ontology_shape: general\nshape: x", "field shape not found"},
		{"forward ref", `refs: ["fact:f01", "fact:f02"]`, `refs: ["fact:f05", "fact:f02"]`, "EARLIER step"},
		{"unlisted source", "refs: [https://records.orchard.invalid/brask-trial]", "refs: [https://elsewhere.invalid/x]", "not listed in external_sources"},
		{"bad move", "{move: refs_out, from: f03}", "{move: teleport, from: f03}", `move "teleport"`},
		{"type for kind", "    kind: pragmatic\n    type: policy\n", "    kind: pragmatic\n    type: observation\n", "not a pragmatic type"},
		{"never learned", "learn: [f07, f08]", "learn: [f07]", "fact f08: no history step learns it"},
		{"bad topic", "topic: rules\n", "topic: nowhere\n", "path nowhere/storage"},
		{"bad motif", "motifs: [one-condition-sets-limit]\n    refs: [https://records.orchard.invalid/cellar-log-2024]", "motifs: [x]\n    refs: [https://records.orchard.invalid/cellar-log-2024]", "motif"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mut := strings.Replace(base, c.from, c.to, 1)
			require.NotEqual(t, base, mut, "the mutation must apply")
			_, err := synthesize.ParseScenario([]byte(mut))
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}
}

var (
	embOnce sync.Once
	embVal  store.BatchEmbedder
	embErr  error
)

// realEmbedder is the production default model, from ~/.knomit/models, so
// the dedup merge and the same-subject gate run as they do for a session.
func realEmbedder(t *testing.T) store.BatchEmbedder {
	t.Helper()
	embOnce.Do(func() {
		model, err := embeddings.Lookup(embeddings.DefaultModelID)
		if err != nil {
			embErr = err
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			embErr = err
			return
		}
		e, err := embeddings.NewEmbedder(context.Background(), model, filepath.Join(home, ".knomit", "models"))
		if err != nil {
			embErr = err
			return
		}
		embVal = e
	})
	require.NoError(t, embErr, "the embedder is needed for the dedup and same-subject gates; set EXPERT_SPEC_EMBED=0 to load without them")
	return embVal
}

// TestScenarioSpecDir loads EVERY spec.yaml under $EXPERT_SPEC_DIR through
// the write path, with the real embedder (dedup merge and same-subject gate
// on) unless EXPERT_SPEC_EMBED=0, and fails on the first world that does not
// load, naming the world, the fact or step, and the rule or gate that refused
// it. This is the command a generation session runs before committing worlds:
//
//	EXPERT_SPEC_DIR=/path/to/expert-scenarios/worlds \
//	  go test -count=1 -run TestScenarioSpecDir -v ./internal/synthesize/
func TestScenarioSpecDir(t *testing.T) {
	dir := os.Getenv("EXPERT_SPEC_DIR")
	if dir == "" {
		t.Skip("set EXPERT_SPEC_DIR to a directory of scenario worlds (spec.yaml files) to validate them")
	}
	specs, err := synthesize.FindScenarioSpecs(dir)
	require.NoError(t, err)
	require.NotEmpty(t, specs, "no %s under %s", synthesize.ScenarioSpecFile, dir)
	var emb store.BatchEmbedder
	if os.Getenv("EXPERT_SPEC_EMBED") != "0" {
		emb = realEmbedder(t)
	}
	seen := map[string]string{}
	for _, p := range specs {
		s, err := synthesize.ReadScenarioFile(p)
		if err != nil {
			t.Fatalf("INVALID %v", err)
		}
		if prev, dup := seen[s.ID]; dup {
			t.Fatalf("INVALID %s: world id %s is also used by %s", p, s.ID, prev)
		}
		seen[s.ID] = p
		w, err := loadSpec(t, s, emb)
		if err != nil {
			t.Fatalf("INVALID %s: %v", p, err)
		}
		if err := synthesize.CheckGold(context.Background(), w); err != nil {
			t.Fatalf("INVALID %s: %v", p, err)
		}
		t.Logf("ok %s: %d facts, %d commits, %d questions", s.ID, len(w.Paths), len(w.StepCommits), len(s.Questions))
	}
}

// TestScenarioExample_PassesTheGates loads the example with the real
// embedder: the planted near-duplicates pass dedup and the same-subject gate
// only because the spec cites or names them (refs, distinct_from).
func TestScenarioExample_PassesTheGates(t *testing.T) {
	if os.Getenv("EXPERT_SPEC_EMBED") != "1" {
		t.Skip("set EXPERT_SPEC_EMBED=1 to load the example with the real embedder")
	}
	s, err := synthesize.ReadScenarioFile(exampleSpec)
	require.NoError(t, err)
	_, err = loadSpec(t, s, realEmbedder(t))
	require.NoError(t, err)
}
