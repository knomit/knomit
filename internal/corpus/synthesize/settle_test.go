package synthesize

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/model"
)

// #641: a verdict on a hypothesis is the commit's Knomit-Outcome trailer, the
// citation and the retraction land in the SAME commit.

const (
	setH = "kb/technology/hyp.md"
	setO = "kb/technology/obs.md"
)

func seedSettle(t *testing.T) (*store.Service, string) {
	t.Helper()
	svc, branch := synthtest.NewSourcesTestRepo(t)
	synthtest.SeedOriginFact(t, svc, branch, setH, fact.Hypothesis, fact.Authored, 0.4, 1, nil)
	synthtest.SeedOriginFact(t, svc, branch, setO, fact.Observation, fact.Authored, 0.8, 1, nil)
	return svc, branch
}

type tip struct {
	hash    string
	message string
	parents []plumbing.Hash
}

func tipOf(t *testing.T, svc *store.Service, branch string) tip {
	t.Helper()
	h, err := svc.HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	info, err := svc.FactsRepo().CommitInfo(context.Background(), plumbing.NewHash(h))
	require.NoError(t, err)
	return tip{hash: h, message: info.Message, parents: info.Parents}
}

func liveAt(t *testing.T, svc *store.Service, branch, path string) bool {
	t.Helper()
	ok, err := svc.Facts().FactExists(context.Background(), branch, path)
	require.NoError(t, err)
	return ok
}

func refsOf(t *testing.T, svc *store.Service, branch, path string) []string {
	t.Helper()
	r, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, r.Content)
	require.NoError(t, err)
	return f.Refs
}

func applyPrune(t *testing.T, svc *store.Service, branch string, ds ...step.PruneDecision) []string {
	t.Helper()
	var warns []string
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(), ds, nil,
		"review-test", func(e step.ProgressEvent) {
			if e.Phase == "warn" {
				warns = append(warns, e.Message)
			}
		}, branch, synthtest.BareRefFixture, "kb", nil)
	require.NoError(t, err)
	return warns
}

// SABOTAGE: drop model.WithOutcomes in applyPruneSettle (no trailer), write
// the refs and the delete as two commits, or skip the refs → red.
func TestApplyPruneDecisions_SettleCitesRetractsAndRecordsInOneCommit(t *testing.T) {
	for _, outcome := range []string{"confirmed", "refuted"} {
		t.Run(outcome, func(t *testing.T) {
			svc, branch := seedSettle(t)
			before := tipOf(t, svc, branch)
			warns := applyPrune(t, svc, branch, step.PruneDecision{Path: setH, Action: "settle", Outcome: outcome, By: []string{setO}})
			require.Empty(t, warns)

			after := tipOf(t, svc, branch)
			require.Equal(t, []plumbing.Hash{plumbing.NewHash(before.hash)}, after.parents, "one commit")
			require.False(t, liveAt(t, svc, branch, setH))
			require.Contains(t, strings.Join(refsOf(t, svc, branch, setO), " "), setH)
			require.Equal(t, []string{setH + " " + outcome}, model.TrailerValues(after.message, model.TrailerOutcome))
		})
	}
}

// SABOTAGE: remove the By-is-a-hypothesis refusal in applyPruneSettle → red.
func TestApplyPruneDecisions_SettleRefusals(t *testing.T) {
	svc, branch := seedSettle(t)
	const h2 = "kb/technology/hyp2.md"
	synthtest.SeedOriginFact(t, svc, branch, h2, fact.Hypothesis, fact.Authored, 0.4, 1, nil)
	before := tipOf(t, svc, branch)

	cases := map[string]struct {
		d    step.PruneDecision
		want string
	}{
		"by a hypothesis": {step.PruneDecision{Path: setH, Action: "settle", Outcome: "confirmed", By: []string{h2}}, "predictions cannot settle predictions"},
		"target not hyp":  {step.PruneDecision{Path: setO, Action: "settle", Outcome: "confirmed", By: []string{setH}}, "is not a hypothesis"},
		"bad outcome":     {step.PruneDecision{Path: setH, Action: "settle", Outcome: "maybe", By: []string{setO}}, "outcome"},
		"no by":           {step.PruneDecision{Path: setH, Action: "settle", Outcome: "refuted"}, "by is empty"},
		"by missing fact": {step.PruneDecision{Path: setH, Action: "settle", Outcome: "refuted", By: []string{"kb/technology/nope.md"}}, "read by"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			warns := applyPrune(t, svc, branch, c.d)
			require.Len(t, warns, 1)
			require.Contains(t, warns[0], c.want)
			require.Equal(t, before.hash, tipOf(t, svc, branch).hash, "a refused settle writes nothing")
			require.True(t, liveAt(t, svc, branch, setH))
		})
	}
}

func TestValidatePrunePaths_Settle(t *testing.T) {
	in := []string{setH, setO}
	ok := step.PruneResult{Decisions: []step.PruneDecision{{Path: setH, Action: "settle", Outcome: "refuted", By: []string{setO}}}}
	require.NoError(t, step.ValidatePrunePaths(ok, in))
	for name, d := range map[string]step.PruneDecision{
		"outcome":    {Path: setH, Action: "settle", Outcome: "x", By: []string{setO}},
		"empty by":   {Path: setH, Action: "settle", Outcome: "refuted"},
		"unknown by": {Path: setH, Action: "settle", Outcome: "refuted", By: []string{"kb/x/nope.md"}},
		"self":       {Path: setH, Action: "settle", Outcome: "refuted", By: []string{setH}},
	} {
		require.Error(t, step.ValidatePrunePaths(step.PruneResult{Decisions: []step.PruneDecision{d}}, in), name)
	}
}

// Distill: the synthesized fact cites the hypothesis; settle retracts it in
// the same commit and records the verdict. A settle no written fact cites
// leaves the hypothesis alone.
//
// SABOTAGE: write the fact and delete as two commits, or drop the trailer → red.
func TestApplyDistillDecisions_SettleRecordsOutcomeInTheSynthesisCommit(t *testing.T) {
	svc, branch := seedSettle(t)
	df := step.DistillFact{Path: "kb/technology/syn.md", Title: "Syn", Body: "claim", Type: "synthesis", Confidence: 0.8, Refs: step.FlexStrings{setH, setO}}
	before := tipOf(t, svc, branch)
	stats, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
		[]step.DistillFact{df}, nil, []step.HypothesisOutcome{{Path: setH, Outcome: "refuted"}},
		"review-test", func(step.ProgressEvent) {}, branch, synthtest.BareRefFixture, "kb")
	require.NoError(t, err)
	require.Len(t, written, 1)
	after := tipOf(t, svc, branch)
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(before.hash)}, after.parents, "one commit")
	require.False(t, liveAt(t, svc, branch, setH))
	require.True(t, liveAt(t, svc, branch, written[0].Path))
	require.Equal(t, []string{setH + " refuted"}, model.TrailerValues(after.message, model.TrailerOutcome))
	require.Equal(t, []string{setH}, stats.Retired)
}

func TestApplyDistillDecisions_SettleNotCitedLeavesTheHypothesis(t *testing.T) {
	svc, branch := seedSettle(t)
	df := step.DistillFact{Path: "kb/technology/syn.md", Title: "Syn", Body: "claim", Type: "synthesis", Confidence: 0.8, Refs: step.FlexStrings{setO}}
	_, _, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
		[]step.DistillFact{df}, nil, []step.HypothesisOutcome{{Path: setH, Outcome: "confirmed"}},
		"review-test", func(step.ProgressEvent) {}, branch, synthtest.BareRefFixture, "kb")
	require.NoError(t, err)
	require.True(t, liveAt(t, svc, branch, setH))
	require.NotContains(t, tipOf(t, svc, branch).message, model.TrailerOutcome)
}

func TestValidateDistillPaths_Settle(t *testing.T) {
	in := []string{setH, setO}
	require.NoError(t, step.ValidateDistillPaths(step.DistillResult{Settle: []step.HypothesisOutcome{{Path: setH, Outcome: "confirmed"}}}, in))
	require.Error(t, step.ValidateDistillPaths(step.DistillResult{Settle: []step.HypothesisOutcome{{Path: setH, Outcome: "x"}}}, in))
	require.Error(t, step.ValidateDistillPaths(step.DistillResult{Settle: []step.HypothesisOutcome{{Path: "kb/nope.md", Outcome: "confirmed"}}}, in))
	require.Error(t, step.ValidateDistillPaths(step.DistillResult{Retract: []string{setH}, Settle: []step.HypothesisOutcome{{Path: setH, Outcome: "confirmed"}}}, in))
}

// dedupCluster: a hypothesis/non-hypothesis pair is treated as learn's
// unjudged subsumption: the survivor cites it, it is retracted in the same
// commit, no outcome is written, and none of its fields transfer.
//
// SABOTAGE: set `subsumed := false` in dedupCluster → two commits, red.
func TestDedupCluster_HypothesisLoserIsCitedAndRetractedInOneCommit(t *testing.T) {
	ctx := context.Background()
	svc, branch := seedSettle(t)
	cluster := []step.FactForLLM{
		{File: setO, Kind: "epistemic", Title: "o", Body: "b", Type: string(fact.Observation), Confidence: 0.8, Sources: 1},
		{File: setH, Kind: "epistemic", Title: "h", Body: "b", Type: string(fact.Hypothesis), Confidence: 0.4, Sources: 1},
	}
	idx := fixedPairSearch(t, svc, []index.SearchResult{searchHit(setO), searchHit(setH)})
	before := tipOf(t, svc, branch)
	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), svc.FactsRepo(), idx, 0.92, "test", func(step.ProgressEvent) {}, branch, synthtest.BareRefFixture)
	require.NoError(t, err)
	require.Len(t, surviving, 1)
	require.Equal(t, setO, surviving[0].File)

	after := tipOf(t, svc, branch)
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(before.hash)}, after.parents, "one commit")
	require.False(t, liveAt(t, svc, branch, setH))
	require.Contains(t, strings.Join(refsOf(t, svc, branch, setO), " "), setH)
	require.NotContains(t, after.message, model.TrailerOutcome)
}

// Reflect reads the verdict: a settled hypothesis is reported "refuted" /
// "confirmed"; a plain retraction stays "retracted".
//
// SABOTAGE: ignore outcomes in findHypothesisTransitions → red.
func TestFindHypothesisTransitions_ReportsTheSettledOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, want string }{{"refuted", "refuted"}, {"confirmed", "confirmed"}, {"plain", "retracted"}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, branch := seedSettle(t)
			head, err := svc.HeadCommit(ctx, branch)
			require.NoError(t, err)
			require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, step.ReviewTool, branch, head))
			if tc.name == "plain" {
				_, err := svc.Facts().DeleteFact(ctx, branch, setH, "retract")
				require.NoError(t, err)
			} else {
				require.Empty(t, applyPrune(t, svc, branch, step.PruneDecision{Path: setH, Action: "settle", Outcome: tc.name, By: []string{setO}}))
			}
			d := step.Deps{Facts: svc.Facts(), Files: svc.FactsRepo(), Pipeline: svc.Pipeline()}
			got, err := findHypothesisTransitions(ctx, d, &model.PipelineSession{Branch: branch})
			require.NoError(t, err)
			require.Equal(t, []hypothesisTransition{{Path: setH, OriginalType: "hypothesis", Action: tc.want}}, got)
		})
	}
}

// B1 (r1): a settle whose `by` fact a merge in the same response consumes
// would leave the hypothesis cited by nothing. Refused with a warn, no write.
//
// SABOTAGE: drop the mergeMembers check in applyPruneSettle → red.
func TestApplyPruneDecisions_SettleRefusesAByFactTheSameResponseMerges(t *testing.T) {
	svc, branch := seedSettle(t)
	const o2 = "kb/technology/obs2.md"
	synthtest.SeedOriginFact(t, svc, branch, o2, fact.Observation, fact.Authored, 0.8, 1, nil)
	var warns []string
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
		[]step.PruneDecision{{Path: setH, Action: "settle", Outcome: "confirmed", By: []string{setO}}},
		[]step.MergeEntry{{Paths: []string{setO, o2}, Merged: step.MergedFact{Path: "kb/technology/m.md", Title: "M", Body: "merged", Confidence: 0.8, Refs: step.FlexStrings{setO, o2}}}},
		"review-test", func(e step.ProgressEvent) {
			if e.Phase == "warn" {
				warns = append(warns, e.Message)
			}
		}, branch, synthtest.BareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.NotEmpty(t, warns)
	require.Contains(t, warns[0], "merged in this same response")
	require.True(t, liveAt(t, svc, branch, setH), "the refused settle leaves the hypothesis live")
}

// S1: the settled path is a precondition under the branch lock. A rival that
// deletes the hypothesis between validation and the write makes the settle
// fail with nothing written (prune and distill).
//
// SABOTAGE: use BatchWriteFacts instead of BatchWriteFactsMustExist → the
// stale settle commits its own verdict → red.
func TestSettle_HypothesisMustStillExistUnderTheLock(t *testing.T) {
	rival := func(t *testing.T, svc *store.Service, branch string) func() {
		var once sync.Once
		return facts.SetBatchWriteUnlockedHookForTest(func() {
			once.Do(func() {
				_, err := svc.Facts().DeleteFact(context.Background(), branch, setH, "rival settle")
				require.NoError(t, err)
			})
		})
	}
	t.Run("prune", func(t *testing.T) {
		svc, branch := seedSettle(t)
		defer rival(t, svc, branch)()
		warns := applyPrune(t, svc, branch, step.PruneDecision{Path: setH, Action: "settle", Outcome: "confirmed", By: []string{setO}})
		require.Len(t, warns, 1)
		require.NotContains(t, tipOf(t, svc, branch).message, model.TrailerOutcome)
		require.NotContains(t, strings.Join(refsOf(t, svc, branch, setO), " "), setH, "nothing written")
	})
	t.Run("distill", func(t *testing.T) {
		svc, branch := seedSettle(t)
		defer rival(t, svc, branch)()
		df := step.DistillFact{Path: "kb/technology/syn.md", Title: "Syn", Body: "claim", Type: "synthesis", Confidence: 0.8, Refs: step.FlexStrings{setH, setO}}
		_, written, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
			[]step.DistillFact{df}, nil, []step.HypothesisOutcome{{Path: setH, Outcome: "refuted"}},
			"review-test", func(step.ProgressEvent) {}, branch, synthtest.BareRefFixture, "kb")
		require.NoError(t, err)
		require.Empty(t, written)
		require.NotContains(t, tipOf(t, svc, branch).message, model.TrailerOutcome)
	})
}

// S3: one warn per settle no written fact settles, also when synthesize is empty.
//
// SABOTAGE: drop the warn loop in ApplyDistillDecisions → red.
func TestApplyDistillDecisions_UnusedSettleWarns(t *testing.T) {
	svc, branch := seedSettle(t)
	var warns []string
	_, _, err := ApplyDistillDecisions(context.Background(), svc.Facts(), svc.FactsRepo(), svc.Search(),
		nil, nil, []step.HypothesisOutcome{{Path: setH, Outcome: "refuted"}},
		"review-test", func(e step.ProgressEvent) {
			if e.Phase == "warn" {
				warns = append(warns, e.Message)
			}
		}, branch, synthtest.BareRefFixture, "kb")
	require.NoError(t, err)
	require.Len(t, warns, 1)
	require.Contains(t, warns[0], setH)
}

// S6: OutcomesSince reads every value per commit, only after the watermark,
// latest word wins, and ignores a value that is not an outcome.
//
// SABOTAGE: read only the last value per commit; take the first sighting; or
// drop the ParseOutcome check → red.
func TestOutcomesSince(t *testing.T) {
	ctx := context.Background()
	svc, branch := synthtest.NewSourcesTestRepo(t)
	write := func(c context.Context, name string) {
		_, err := svc.Facts().WriteFact(c, branch, "kb/technology/"+name+".md", "---\ntype: observation\n---\n# T\n\nb", name, "")
		require.NoError(t, err)
	}
	write(model.WithOutcomes(ctx, []model.Outcome{{Path: "kb/x/old.md", Outcome: "confirmed"}}), "before")
	head, err := svc.HeadCommit(ctx, branch)
	require.NoError(t, err)

	write(model.WithOutcomes(ctx, []model.Outcome{{Path: "kb/x/p1.md", Outcome: "confirmed"}, {Path: "kb/x/p2.md", Outcome: "refuted"}}), "two")
	write(model.WithOutcomes(ctx, []model.Outcome{{Path: "kb/x/p1.md", Outcome: "refuted"}}), "resettle")
	write(model.WithTrailers(ctx, model.Trailers{Extra: []model.TrailerEntry{{Key: model.TrailerOutcome, Value: "kb/x/p3.md maybe"}}}), "garbage")

	got, err := svc.FactsRepo().OutcomesSince(ctx, branch, head)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"kb/x/p1.md": "refuted", "kb/x/p2.md": "refuted"}, got)
}
