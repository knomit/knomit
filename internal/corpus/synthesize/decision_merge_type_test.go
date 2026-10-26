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
	_, err := env.Svc.Facts().WriteFact(context.Background(), env.Branch, path,
		fmt.Sprintf("---\ntype: %s\nconfidence: %.2f\nsources: 1\n---\n# %s\n\nbody of %s", typ, conf, title, title),
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
