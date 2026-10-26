package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/model"
)

// knomit_learn `settles`: a fact names the hypotheses it confirms or refutes.
// Each is cited, retracted in the same commit, and the verdict is the commit's
// Knomit-Outcome trailer. Fixture: `notes` has dedup ON, `inbox` OFF.

func withSettles(f map[string]any, pairs ...string) map[string]any {
	var ss []any
	for i := 0; i+1 < len(pairs); i += 2 {
		ss = append(ss, map[string]any{"path": pairs[i], "outcome": pairs[i+1]})
	}
	f["settles"] = ss
	return f
}

func seedHyp(t *testing.T, ctx context.Context, emb model.BatchEmbedder, topic, cat, title string) string {
	t.Helper()
	return settledWritten(t, settledLearn(t, ctx, emb, "seed-"+cat, nil, settledFact(topic, cat, title, "hypothesis")))[0]
}

func settleErr(t *testing.T, ctx context.Context, emb model.BatchEmbedder, args map[string]any) string {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	r, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.True(t, r.IsError, resultText(t, r))
	return resultText(t, r)
}

// SABOTAGE: drop model.WithOutcomes (no trailer), drop the Refs append in
// applySettles, or drop the retract → red. Run once per verdict.
func TestLearnSettles_RecordsOutcomeCitesAndRetracts(t *testing.T) {
	for _, outcome := range []string{"confirmed", "refuted"} {
		t.Run(outcome, func(t *testing.T) {
			ri, ctx, emb := newDedupAttrRepo(t)
			// inbox: dedup OFF, so similarity plays no part.
			hyp := seedHyp(t, ctx, emb, "inbox", "a", "Prediction A")
			res := settledLearn(t, ctx, emb, "settle", nil, withSettles(settledFact("inbox", "b", "Different thing", "observation"), hyp, outcome))
			obs := settledWritten(t, res)[0]

			require.Equal(t, []string{"fact 0: hypothesis " + hyp + " " + outcome + "; retracted, cited in refs."}, settledNotesOf(res))
			require.Equal(t, "learn: settle\n\nKnomit-Outcome: "+hyp+" "+outcome, strings.TrimSpace(settledMessage(t, ri, "agent/test")))
			require.False(t, factExistsOn(t, ri, "agent/test", hyp))
			require.True(t, factExistsOn(t, ri, "agent/test", obs))
			require.Contains(t, readFactAt(t, ri, obs).Refs[0], hyp)
			require.NotContains(t, res, "retracted", "retracted keeps its F04 meaning")
			require.NotContains(t, readFactAt(t, ri, obs).Body, "settles")
		})
	}
}

// Two hypotheses on one fact, one on another: one trailer line each, in the
// LAST paragraph with a caller's trace; the notes are per fact and hypothesis.
//
// SABOTAGE: stamp the outcomes in the message body instead of the trailer set
// (so the trace paragraph follows them) → red.
func TestLearnSettles_SeveralAndTraceShareTheLastParagraph(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	h0 := seedHyp(t, ctx, emb, "inbox", "a", "Prediction A")
	h1 := seedHyp(t, ctx, emb, "inbox", "b", "Prediction B")
	h2 := seedHyp(t, ctx, emb, "inbox", "c", "Prediction C")
	ctx = model.WithTrailers(ctx, model.Trailers{Trace: "story-1"})
	res := settledLearn(t, ctx, emb, "many", nil,
		withSettles(settledFact("inbox", "x", "Obs X", "observation"), h0, "confirmed", h1, "refuted"),
		withSettles(settledFact("inbox", "y", "Obs Y", "observation"), h2, "refuted"))
	require.Len(t, settledNotesOf(res), 3)
	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, "learn: many\n\nKnomit-Trace: story-1\nKnomit-Outcome: "+h0+" confirmed\nKnomit-Outcome: "+h1+" refuted\nKnomit-Outcome: "+h2+" refuted\n", msg)
	require.Equal(t, []string{h0 + " confirmed", h1 + " refuted", h2 + " refuted"}, model.TrailerValues(msg, model.TrailerOutcome))
}

// Without settles the similarity subsumption is unchanged and records NO
// outcome: there is no verdict.
//
// SABOTAGE: stamp an outcome for the similarity subsumption → red.
func TestLearnSettles_UndeclaredSubsumptionHasNoOutcome(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := seedHyp(t, ctx, emb, "notes", "a", "Prediction A")
	res := settledLearn(t, ctx, emb, "settle", nil, settledFact("notes", "a", "Prediction A", "observation"))
	obs := settledWritten(t, res)[0]
	require.Equal(t, []string{settledNoteFor(0, hyp, obs)}, settledNotesOf(res))
	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, "learn: settle\n\nsettled: "+hyp, strings.TrimSpace(msg))
	require.NotContains(t, msg, model.TrailerOutcome)
}

// The declared outcome wins over similarity: a fact that matches its
// hypothesis above Dedup AND declares it is reported once, with the verdict,
// and gets no `settled:` line.
//
// SABOTAGE: drop `retracting[...] = true` in applySettles → the merge stage
// subsumes it too and the extra note and settled line appear → red.
func TestLearnSettles_DeclaredOutcomeOverridesSimilarity(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := seedHyp(t, ctx, emb, "notes", "a", "Prediction A")
	res := settledLearn(t, ctx, emb, "settle", nil, withSettles(settledFact("notes", "a", "Prediction A", "observation"), hyp, "refuted"))
	obs := settledWritten(t, res)[0]
	require.NotEqual(t, hyp, obs)
	require.Equal(t, []string{"fact 0: hypothesis " + hyp + " refuted; retracted, cited in refs."}, settledNotesOf(res))
	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, "learn: settle\n\nKnomit-Outcome: "+hyp+" refuted", strings.TrimSpace(msg))
	require.NotContains(t, msg, "settled:")
	require.False(t, factExistsOn(t, ri, "agent/test", hyp))
}

// SABOTAGE: remove any one refusal in applySettles → its case goes red.
func TestLearnSettles_Refusals(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := seedHyp(t, ctx, emb, "inbox", "a", "Prediction A")
	other := seedHyp(t, ctx, emb, "inbox", "z", "Prediction Z")
	notHyp := settledWritten(t, settledLearn(t, ctx, emb, "seed-obs", nil, settledFact("inbox", "o", "An observation", "observation")))[0]
	before := headOf(t, ri, "agent/test")

	obs := func(pairs ...string) map[string]any {
		return withSettles(settledFact("inbox", "b", "Settler", "observation"), pairs...)
	}
	call := func(f ...map[string]any) map[string]any {
		fs := make([]any, len(f))
		for i := range f {
			fs[i] = f[i]
		}
		return map[string]any{"moment_name": "m", "facts": fs}
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"bad outcome", call(obs(hyp, "maybe")), `outcome "maybe"`},
		{"not a hypothesis", call(obs(notHyp, "confirmed")), "is not a hypothesis"},
		{"missing path", call(obs("kb/inbox/a/nope.md", "confirmed")), "not found"},
		{"empty path", call(obs("", "confirmed")), "path is empty"},
		{"hypothesis settles", call(withSettles(settledFact("inbox", "b", "Settler", "hypothesis"), hyp, "confirmed")), "a hypothesis cannot settle a hypothesis"},
		{"in distinct_from", call(withSettles(settledFact("inbox", "b", "Settler", "observation", hyp), hyp, "confirmed")), "also in this fact's distinct_from"},
		{"conflicting verdicts", call(obs(hyp, "confirmed"), withSettles(settledFact("inbox", "c", "Settler 2", "observation"), hyp, "refuted")), "both confirmed and refuted"},
		{"top level", map[string]any{"moment_name": "m", "facts": call(obs())["facts"], "settles": []any{}}, "settles belongs on each fact"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Contains(t, settleErr(t, ctx, emb, c.args), c.want)
		})
	}
	t.Run("in retract", func(t *testing.T) {
		a := call(obs(hyp, "confirmed"))
		a["retract"] = []any{hyp}
		require.Contains(t, settleErr(t, ctx, emb, a), "also in retract")
	})
	require.Equal(t, before, headOf(t, ri, "agent/test"), "every refusal writes nothing")
	require.True(t, factExistsOn(t, ri, "agent/test", hyp))
	require.True(t, factExistsOn(t, ri, "agent/test", other))
}

// Inside an experiment the verdict, the retraction and the citation land on
// the experiment; the agent branch is untouched.
//
// SABOTAGE: as TestLearnSettles_RecordsOutcomeCitesAndRetracts → red.
func TestLearnSettles_InsideAnExperiment(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := seedHyp(t, ctx, emb, "inbox", "a", "Prediction A")
	branch := openTestExperiment(t, ri, "settling")
	agentBefore := headOf(t, ri, "agent/test")
	ectx := repos.WithBinding(ctx, repos.NewBindingOfRepo(ri, branch))

	res := settledLearn(t, ectx, emb, "settle", nil, withSettles(settledFact("inbox", "b", "Obs", "observation"), hyp, "refuted"))
	require.Len(t, settledNotesOf(res), 1)
	require.Equal(t, "learn: settle\n\nKnomit-Outcome: "+hyp+" refuted", strings.TrimSpace(settledMessage(t, ri, branch)))
	require.False(t, factExistsOn(t, ri, branch, hyp))
	require.True(t, factExistsOn(t, ri, "agent/test", hyp))
	require.Equal(t, agentBefore, headOf(t, ri, "agent/test"))
}

// S2: a second fact whose nearest match is a hypothesis another fact settles
// gets one note, not a silent decline and not a citation.
//
// SABOTAGE: drop the settledBy note in applyDedupMerge → red.
func TestLearnSettles_SecondMatchingFactGetsANote(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	h := seedHyp(t, ctx, emb, "notes", "a", "Prediction A")
	res := settledLearn(t, ctx, emb, "two", nil,
		settledFact("notes", "a", "Prediction A", "observation"),
		withSettles(settledFact("inbox", "b", "Other obs", "observation"), h, "refuted"))
	w := settledWritten(t, res)
	require.Equal(t, []string{
		"fact 0: matched hypothesis " + h + ", settled by fact 1 in this call.",
		"fact 1: hypothesis " + h + " refuted; retracted, cited in refs.",
	}, settledNotesOf(res))
	require.Empty(t, readFactAt(t, ri, w[0]).Refs, "no citation for the second fact")
}

// S1: the settled hypothesis must still exist under the branch lock.
//
// SABOTAGE: drop settleRetract from mustExist → the stale settle commits → red.
func TestLearnSettles_HypothesisMustStillExistUnderTheLock(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	svc := testRepoService(t, ctx)
	hyp := seedHyp(t, ctx, emb, "inbox", "a", "Prediction A")
	before := headOf(t, ri, "agent/test")
	var once sync.Once
	defer facts.SetBatchWriteUnlockedHookForTest(func() {
		once.Do(func() {
			_, err := svc.Facts().DeleteFact(context.Background(), "agent/test", hyp, "rival")
			require.NoError(t, err)
		})
	})()
	msg := settleErr(t, ctx, emb, map[string]any{"moment_name": "m", "facts": []any{
		withSettles(settledFact("inbox", "b", "Obs", "observation"), hyp, "refuted")}})
	require.Contains(t, msg, "nothing written")
	require.NotContains(t, settledMessage(t, ri, "agent/test"), model.TrailerOutcome)
	_ = before
}
