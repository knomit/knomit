package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// F03 merge rule, synthesis side: a surviving fact keeps its OWN expires; a
// merge or a new claim never invents, inherits, or pools one.

const (
	expA = "2026-10-01T00:00:00Z"
	expB = "2027-03-01T00:00:00Z"
)

func seedDated(t *testing.T, svc *store.Service, branch, path string, typ fact.Type, expires string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = typ
	f.Domain = []string{"test"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Expires = expires
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

func expiresOf(t *testing.T, svc *store.Service, branch, path string) string {
	t.Helper()
	return readFactForTest(t, svc, branch, path).Expires
}

// TestApplyPruneDecisions_MergedFactCarriesNoExpires (ruling (a)): a prune
// merge writes a NEW fact from its members, so there is no survivor and the
// merged fact has no expires. Each member's date is named in a warn.
func TestApplyPruneDecisions_MergedFactCarriesNoExpires(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	ctx := context.Background()
	seedDated(t, svc, branch, "kb/technology/a.md", fact.Observation, expA)
	seedDated(t, svc, branch, "kb/technology/b.md", fact.Hypothesis, expB)

	sink, warns := collectWarns()
	merges := []MergeEntry{{
		Paths:  []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{Path: "kb/technology/m.md", Title: "Merged", Body: "merged body", Type: "observation"},
	}}
	stats, err := ApplyPruneDecisions(ctx, svc.Facts(), svc.Search(), nil, merges,
		"review-test", sink, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged, "fixture: the merge must have happened")

	merged := mergedFactPath(t, svc, branch, "Merged")
	require.Equal(t, "", expiresOf(t, svc, branch, merged), "a merge never invents or inherits an expiry")

	var got string
	for _, w := range *warns {
		if strings.Contains(w, "expires not carried") {
			got = w
		}
	}
	require.NotEmpty(t, got, "the dropped member dates must be reported: %v", *warns)
	require.Contains(t, got, "kb/technology/a.md (expires "+expA+")")
	require.Contains(t, got, "kb/technology/b.md (expires "+expB+")")
}

// TestApplyPruneDecisions_UndatedMembersNoWarn: nothing to report → no warn.
func TestApplyPruneDecisions_UndatedMembersNoWarn(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	seedDated(t, svc, branch, "kb/technology/a.md", fact.Observation, "")
	seedDated(t, svc, branch, "kb/technology/b.md", fact.Observation, "")
	sink, warns := collectWarns()
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, []MergeEntry{{
		Paths:  []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{Path: "kb/technology/m.md", Title: "Merged", Body: "merged body", Type: "observation"},
	}}, "review-test", sink, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	for _, w := range *warns {
		require.NotContains(t, w, "expires not carried")
	}
}

// TestApplyDistillDecisions_NewClaimInheritsNoExpires: a distilled synthesis
// is a new claim over inputs that stay alive with their own dates.
func TestApplyDistillDecisions_NewClaimInheritsNoExpires(t *testing.T) {
	svc, branch := newSourcesTestRepo(t)
	seedDated(t, svc, branch, "kb/technology/a.md", fact.Hypothesis, expA)
	seedDated(t, svc, branch, "kb/technology/b.md", fact.Observation, expB)
	df := distillFact{
		Path: "kb/technology/synth.md", Title: "S", Body: "distilled", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9,
		Refs: []string{"kb/technology/a.md", "kb/technology/b.md"},
	}
	_, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.Search(), []distillFact{df}, nil,
		"test", func(ProgressEvent) {}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1)
	require.Equal(t, "", expiresOf(t, svc, branch, written[0].Path))
	require.Equal(t, expA, expiresOf(t, svc, branch, "kb/technology/a.md"), "inputs keep their own")
}

// TestApplyReflectDecisions_NewMethodologyInheritsNoExpires.
func TestApplyReflectDecisions_NewMethodologyInheritsNoExpires(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	branch := "agent/test"
	seedDated(t, svc, branch, "kb/technology/a.md", fact.Hypothesis, expA)
	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", branch, "")
	require.NoError(t, err)
	result := ReflectResult{Propose: []ProposeEntry{{
		TopicPath: "meta/reasoning", Title: "M", Body: "a methodology",
		NoveltyArgument: "nothing like it exists", Confidence: 0.8,
		TransitionPaths: []string{"kb/technology/a.md"},
		Refs:            []string{"kb/technology/a.md"},
	}}}
	require.NoError(t, ApplyReflectDecisions(ctx, svc.Facts(), svc.Search(), result, sess,
		bareRefFixture, ri.OntologyRoot(), 0.95, nil))
	found := methodologyFactPaths(t, svc, branch)
	require.Len(t, found, 1, "precondition: the propose arm must have written one methodology")
	require.Equal(t, "", expiresOf(t, svc, branch, found[0]))
}

// TestApplyDiscoveredProposals_NewClaimInheritsNoExpires.
func TestApplyDiscoveredProposals_NewClaimInheritsNoExpires(t *testing.T) {
	svc, branch, payload := threeSeedProposalEnv(t)
	seedDated(t, svc, branch, "kb/a.md", fact.Hypothesis, expA)
	seedDated(t, svc, branch, "kb/b.md", fact.Observation, expB)
	props := []DiscoveredFact{{
		Path: "kb/kept.md", Title: "kept", Body: "kept", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9,
		Refs: []string{"kb/a.md", "kb/b.md"},
	}}
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(),
		nil, payload, props, DiscoveryGates{}, branch, bareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1)
	require.Equal(t, "", readFactForTest(t, svc, branch, written[0]).Expires)
}

// TestDedupCluster_WinnerKeepsItsOwnExpires: review's pairwise dedup HAS a
// survivor — it re-reads and re-serializes the winner — so the winner keeps
// its own value and never takes the loser's. Mixed rows: winner dated, and
// winner undated with a dated loser.
func TestDedupCluster_WinnerKeepsItsOwnExpires(t *testing.T) {
	for _, tc := range []struct {
		name, winnerExp, loserExp, want string
	}{
		{"winner keeps its own", expA, expB, expA},
		{"undated winner does not take the loser's", "", expB, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			emb := &restatementEmbedder{vectorFor: func(string) []float32 { return axisVector(0) }}
			env := newRestatementEnvWith(t, 0, emb)
			const winnerPath, loserPath = "kb/alpha/winner.md", "kb/alpha/loser.md"
			write := func(path, title string, conf float64, exp string) {
				f := fact.NewFact(path)
				f.Title, f.Body, f.Type = title, "recording", fact.Observation
				f.Domain, f.Entities, f.Refs = []string{"alpha"}, []string{"Widget"}, []string{}
				f.Confidence, f.Sources, f.Expires = conf, 1, exp
				body, err := fact.SerializeFact(f)
				require.NoError(t, err)
				_, err = env.svc.Facts().WriteFact(ctx, env.branch, path, body, "write", "test")
				require.NoError(t, err)
			}
			write(winnerPath, "Widget fails closed", 0.9, tc.winnerExp)
			write(loserPath, "Widget fails closed again", 0.5, tc.loserExp)
			cluster := []factForLLM{
				{File: winnerPath, Kind: "epistemic", Title: "Widget fails closed", Body: "recording", Type: "observation", Confidence: 0.9, Sources: 1},
				{File: loserPath, Kind: "epistemic", Title: "Widget fails closed again", Body: "recording", Type: "observation", Confidence: 0.5, Sources: 1},
			}
			d := env.deps()
			surviving, err := dedupCluster(ctx, cluster, env.svc.Facts(), env.svc.Search(),
				env.dedupThreshold(), reviewTool, d.OnProgress, env.branch, fact.ID12(env.ri.ID()))
			require.NoError(t, err)
			require.Len(t, surviving, 1, "fixture: the pair must have merged")
			require.Equal(t, winnerPath, surviving[0].File)
			require.Equal(t, tc.want, expiresOf(t, env.svc, env.branch, winnerPath))
		})
	}
}
