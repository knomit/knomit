package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// The wiki-link rule on the review pipeline's writes: a distilled fact is a
// new body: none in prose, whatever the members of a prune merge held.

func TestApplyDistillDecisions_WikiLinkInProseRejected(t *testing.T) {
	svc, branch := synthtest.NewSourcesTestRepo(t)
	var warns []string
	onProgress := func(e step.ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}
	bad := step.DistillFact{Path: "kb/technology/bad.md", Title: "Bad", Body: "see [[f6db3a49]]", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9}
	good := step.DistillFact{Path: "kb/technology/good.md", Title: "Good", Body: "plain, with `[[code]]`", Type: "synthesis",
		Domain: []string{"technology"}, Confidence: 0.9}
	_, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
		[]step.DistillFact{bad, good}, nil, nil, "test", onProgress, branch, synthtest.BareRefFixture, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1, "only the plain body is written")
	require.Equal(t, "Good", written[0].Title)
	require.Len(t, warns, 1, "%v", warns)
	require.Contains(t, warns[0], "distill kb/technology/bad.md rejected")
	require.Contains(t, warns[0], "[[f6db3a49]]")
}

// A proposed methodology is a new body; reflect is all-or-nothing, so the
// whole apply is refused.
func TestApplyReflectDecisions_WikiLinkInProseRefused(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	branch := "agent/test"
	seedDated(t, svc, branch, "kb/technology/a.md", fact.Hypothesis, expA)
	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "review", branch, "")
	require.NoError(t, err)
	propose := func(body string) error {
		result := step.ReflectResult{Propose: []step.ProposeEntry{{
			TopicPath: "meta/reasoning", Title: "M " + body, Body: body,
			NoveltyArgument: "nothing like it exists", Confidence: 0.8,
			TransitionPaths: []string{"kb/technology/a.md"},
			Refs:            []string{"kb/technology/a.md"},
		}}}
		return ApplyReflectDecisions(ctx, svc.Facts(), svc.FactsRepo(), svc.Search(), result, sess, "",
			synthtest.BareRefFixture, ri.OntologyRoot(), 0.95, nil)
	}
	err = propose("a methodology, after [[a1b2c3d4]]")
	require.Error(t, err)
	require.Contains(t, err.Error(), "propose[0]: wiki link in fact body")
	require.Empty(t, methodologyFactPaths(t, svc, branch), "a refused reflect writes nothing")

	require.NoError(t, propose("a methodology, plain"))
	require.Len(t, methodologyFactPaths(t, svc, branch), 1)
}

func TestApplyPruneDecisions_MergeMayNotKeepWikiLinks(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		written bool
	}{
		"keeps the members' one": {"merged, see [[a1b2c3d4]]", false},
		"drops it":               {"merged, plain", true},
		"swaps it for another":   {"merged, see [[new]]", false},
		"keeps it in code":       {"merged, see `[[a1b2c3d4]]`", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			svc, err := store.Open(filepath.Join(dir, "k.db"))
			require.NoError(t, err)
			defer svc.Close()
			require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
			for p, body := range map[string]string{"kb/synth/a.md": "claim [[a1b2c3d4]]", "kb/synth/b.md": "claim"} {
				_, err = svc.Facts().WriteFact(context.Background(), "agent/test", p,
					"---\ntype: synthesis\n---\n# S\n\n"+body, "seed "+p, "test")
				require.NoError(t, err)
			}
			var warns []string
			onProgress := func(e step.ProgressEvent) {
				if e.Phase == "warn" {
					warns = append(warns, e.Message)
				}
			}
			merges := []step.MergeEntry{{
				Paths:  []string{"kb/synth/a.md", "kb/synth/b.md"},
				Merged: step.MergedFact{Path: "kb/synth/merged.md", Title: "Merged", Body: tc.body, Type: "synthesis"},
			}}
			stats, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
				nil, merges, "review-test", onProgress, "agent/test", synthtest.BareRefFixture, "kb", nil)
			require.NoError(t, err)
			if tc.written {
				require.Equal(t, 1, stats.Merged, "%v", warns)
				return
			}
			require.Equal(t, 0, stats.Merged)
			require.Len(t, warns, 1, "%v", warns)
			require.True(t, strings.Contains(warns[0], "rejected") && strings.Contains(warns[0], "the body contains [["), warns[0])
		})
	}
}
