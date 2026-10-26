package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
)

// A prune merge never changes a stored fact's type: the merged fact's type
// comes from its members (shared type, else the dedup tiebreak winner's), and
// whatever the judge says is ignored and reported.

func writeTyped(t *testing.T, env *synthtest.RestatementEnv, path, title, typ string, conf float64) {
	t.Helper()
	writeFrontmatter(t, env, path, title, typ, "", conf, 1)
}

func writeFrontmatter(t *testing.T, env *synthtest.RestatementEnv, path, title, typ, kind string, conf float64, sources int) {
	t.Helper()
	kindLine := ""
	if kind != "" {
		kindLine = "kind: " + kind + "\n"
	}
	_, err := env.Svc.Facts().WriteFact(context.Background(), env.Branch, path,
		fmt.Sprintf("---\n%stype: %s\nconfidence: %.2f\nsources: %d\n---\n# %s\n\nbody of %s", kindLine, typ, conf, sources, title, title),
		"write "+path, "test")
	require.NoError(t, err)
}

// mergeTyped merges a and b with a judge that claims judgeType ("" = emits none)
// and returns the written type and every warn.
func mergeTyped(t *testing.T, env *synthtest.RestatementEnv, a, b, judgeType string) (stats *step.ReviewStats, gotType string, warns []string) {
	t.Helper()
	mf := step.MergedFact{
		Path: "kb/alpha/merged-result.md", Title: mergedTitle, Body: "Both, unioned.",
		Type: judgeType, Confidence: 0.9,
	}
	onProgress := func(e step.ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}
	stats, err := ApplyPruneDecisions(context.Background(), env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
		nil, []step.MergeEntry{{Paths: []string{a, b}, Merged: mf}},
		step.ReviewTool, onProgress, env.Branch, "", "kb", nil)
	require.NoError(t, err)
	if stats.Merged == 0 {
		return stats, "", warns
	}
	got, err := env.Svc.Search().GetByPath(context.Background(), env.Branch, mergedFactPath(t, env.Svc, env.Branch, mergedTitle))
	require.NoError(t, err)
	require.NotNil(t, got)
	return stats, string(got.Type), warns
}

func hasWarn(warns []string, parts ...string) bool {
	for _, w := range warns {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestApplyPruneMerge_JudgeTypeIsIgnoredWhenMembersShareOne(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/one.md", "One", "concept", 0.7)
	writeTyped(t, env, "kb/alpha/two.md", "Two", "concept", 0.7)

	stats, got, warns := mergeTyped(t, env, "kb/alpha/one.md", "kb/alpha/two.md", "synthesis")
	require.Equal(t, 1, stats.Merged, "fixture: the merge must apply; warns: %v", warns)
	require.Equal(t, "concept", got, "the members' shared type survives a judge that says synthesis")
	require.True(t, hasWarn(warns, "judge type", `"synthesis"`, `"concept"`),
		"the judge's differing type must be recorded; warns: %v", warns)
}

func TestApplyPruneMerge_NoJudgeTypeIsAccepted_AndNotReported(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/one.md", "One", "concept", 0.7)
	writeTyped(t, env, "kb/alpha/two.md", "Two", "concept", 0.7)

	stats, got, warns := mergeTyped(t, env, "kb/alpha/one.md", "kb/alpha/two.md", "")
	require.Equal(t, 1, stats.Merged, "a merge without a type is no longer rejected; warns: %v", warns)
	require.Equal(t, "concept", got)
	require.False(t, hasWarn(warns, "judge type"), "no judge type, nothing to report; warns: %v", warns)
}

func TestApplyPruneMerge_MixedTypes_NonHypothesisWinsDespiteLowerConfidence(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/hyp.md", "Hyp", "hypothesis", 0.95)
	writeTyped(t, env, "kb/alpha/obs.md", "Obs", "observation", 0.40)

	// The judge, following the old prompt, "promotes" to synthesis.
	stats, got, warns := mergeTyped(t, env, "kb/alpha/hyp.md", "kb/alpha/obs.md", "synthesis")
	require.Equal(t, 1, stats.Merged, "warns: %v", warns)
	require.Equal(t, "observation", got, "non-hypothesis beats hypothesis regardless of confidence (02422cde)")
	require.True(t, hasWarn(warns, "judge type", `"synthesis"`, `"observation"`), "warns: %v", warns)
}

func TestApplyPruneMerge_MixedNonHypothesisTypes_HigherConfidenceMemberTypeWins(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/a.md", "A", "concept", 0.60)
	writeTyped(t, env, "kb/alpha/b.md", "B", "pattern", 0.90)

	// The judge sides with the LOSER's type; the tiebreak must still decide.
	stats, got, warns := mergeTyped(t, env, "kb/alpha/a.md", "kb/alpha/b.md", "concept")
	require.Equal(t, 1, stats.Merged, "warns: %v", warns)
	require.Equal(t, "pattern", got)
	require.True(t, hasWarn(warns, "judge type", `"concept"`, `"pattern"`), "warns: %v", warns)
}

func TestApplyPruneMerge_AllHypothesisMembersAreRefused(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/a.md", "A", "hypothesis", 0.6)
	writeTyped(t, env, "kb/alpha/b.md", "B", "hypothesis", 0.7)

	// The judge claims a non-hypothesis type; the members decide.
	stats, _, warns := mergeTyped(t, env, "kb/alpha/a.md", "kb/alpha/b.md", "synthesis")
	require.Equal(t, 0, stats.Merged, "hypothesis members must not become a non-hypothesis by the judge's word")
	require.True(t, hasWarn(warns, "hypothesis"), "warns: %v", warns)
}

func TestApplyPruneMerge_UnreadableMembersAreRefused(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	stats, _, warns := mergeTyped(t, env, "kb/alpha/gone1.md", "kb/alpha/gone2.md", "synthesis")
	require.Equal(t, 0, stats.Merged, "with no member to read there is no type to keep")
	require.True(t, hasWarn(warns, "cannot be derived"), "warns: %v", warns)
}

// One judge response may both retract members and merge them. The members were
// typed from a snapshot taken before the retracts, so the merge must be
// refused when apply-time reads find none of them.
func TestApplyPruneMerge_MembersRetractedInTheSameCallAreRefused(t *testing.T) {
	env := synthtest.NewRestatementEnv(t, 0)
	writeTyped(t, env, "kb/alpha/one.md", "One", "concept", 0.7)
	writeTyped(t, env, "kb/alpha/two.md", "Two", "concept", 0.7)

	var warns []string
	onProgress := func(e step.ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}
	stats, err := ApplyPruneDecisions(context.Background(), env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
		[]step.PruneDecision{
			{Path: "kb/alpha/one.md", Action: "retract"},
			{Path: "kb/alpha/two.md", Action: "retract"},
		},
		[]step.MergeEntry{{
			Paths:  []string{"kb/alpha/one.md", "kb/alpha/two.md"},
			Merged: step.MergedFact{Path: "kb/alpha/merged-result.md", Title: mergedTitle, Body: "Both.", Confidence: 0.9},
		}},
		step.ReviewTool, onProgress, env.Branch, "", "kb", nil)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Pruned, "fixture: both retracts must have applied")
	require.Equal(t, 0, stats.Merged, "a merge over members already gone must not write a fact from stale types")
	require.True(t, hasWarn(warns, "apply time"), "warns: %v", warns)
}

// Under a widened [cluster_cache] neighbor_kinds a prune merge can hold a
// pragmatic member. Whichever wins the tiebreak, the merge is refused: a policy
// must not be deleted into an epistemic fact (94172e81).
func TestApplyPruneMerge_CrossKindMembersAreRefused_WhoeverWinsTheTiebreak(t *testing.T) {
	for _, tc := range []struct {
		name           string
		obsConf, polCf float64
	}{
		{"policy wins", 0.5, 0.7},
		{"observation wins", 0.9, 0.7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := synthtest.NewRestatementEnv(t, 0)
			writeFrontmatter(t, env, "kb/alpha/obs.md", "Obs", "observation", "", tc.obsConf, 1)
			writeFrontmatter(t, env, "kb/alpha/pol.md", "Pol", "policy", "pragmatic", tc.polCf, 1)

			stats, _, warns := mergeTyped(t, env, "kb/alpha/obs.md", "kb/alpha/pol.md", "observation")
			require.Equal(t, 0, stats.Merged, "warns: %v", warns)
			require.True(t, hasWarn(warns, "not all of one known kind"), "warns: %v", warns)
			for _, p := range []string{"kb/alpha/obs.md", "kb/alpha/pol.md"} {
				ok, err := env.Svc.Facts().FactExists(context.Background(), env.Branch, p)
				require.NoError(t, err)
				require.True(t, ok, "%s must survive a refused merge", p)
			}
		})
	}
}

// With three tied members the winner must not depend on how the fold pooled
// sources: sources [2,2,3] at equal confidence, the third has the most.
func TestApplyPruneMerge_ThreeWayTieUsesEachMembersOwnSources(t *testing.T) {
	for _, order := range [][]string{{"a", "b", "c"}, {"c", "b", "a"}, {"b", "c", "a"}} {
		env := synthtest.NewRestatementEnv(t, 0)
		kinds := map[string]struct {
			typ     string
			sources int
		}{"a": {"concept", 2}, "b": {"pattern", 2}, "c": {"process", 3}}
		var paths []string
		for _, k := range order {
			p := "kb/alpha/" + k + ".md"
			writeFrontmatter(t, env, p, "T"+k, kinds[k].typ, "", 0.8, kinds[k].sources)
			paths = append(paths, p)
		}
		stats, err := ApplyPruneDecisions(context.Background(), env.Svc.Facts(), env.Svc.FactsRepo(), env.Svc.Search(),
			nil, []step.MergeEntry{{Paths: paths, Merged: step.MergedFact{
				Path: "kb/alpha/merged-result.md", Title: mergedTitle, Body: "All.", Confidence: 0.9}}},
			step.ReviewTool, func(step.ProgressEvent) {}, env.Branch, "", "kb", nil)
		require.NoError(t, err)
		require.Equal(t, 1, stats.Merged, "order %v", order)
		got, err := env.Svc.Search().GetByPath(context.Background(), env.Branch, mergedFactPath(t, env.Svc, env.Branch, mergedTitle))
		require.NoError(t, err)
		require.Equal(t, "process", string(got.Type), "order %v: the member with the most sources wins a confidence tie", order)
	}
}
