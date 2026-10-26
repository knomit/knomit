package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
)

// An observation that subsumes a stored hypothesis deletes it as a side effect
// of knomit_learn. The call must say so: ONE short note per fact naming the
// path, ONE short "settled:" commit line per call, and nothing else changes —
// no new response field, and `retracted` keeps its F04 meaning (the caller's
// own retract paths only).
//
// The fixture is the learn_dedup fixture: `notes` has dedup ON, `inbox` OFF.

func settledFact(topic, category, title, typ string, distinct ...string) map[string]any {
	ds := []any{}
	for _, d := range distinct {
		ds = append(ds, d)
	}
	return map[string]any{
		"topic": topic, "category": category, "title": title,
		"body": "Body of " + title, "type": typ, "confidence": 0.7, "sources": 1,
		"domain": []any{"queue"}, "entities": []any{"settle-" + category}, "refs": []any{},
		"distinct_from": ds,
	}
}

func settledLearn(t *testing.T, ctx context.Context, emb model.BatchEmbedder, moment string, retract []string, facts ...map[string]any) map[string]any {
	t.Helper()
	fs := make([]any, len(facts))
	for i, f := range facts {
		fs[i] = f
	}
	rs := make([]any, len(retract))
	for i, r := range retract {
		rs[i] = r
	}
	var req mcpgo.CallToolRequest
	args := map[string]any{"moment_name": moment, "facts": fs}
	if len(rs) > 0 {
		args["retract"] = rs
	}
	req.Params.Arguments = args
	r, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &out))
	return out
}

func settledWritten(t *testing.T, res map[string]any) []string {
	t.Helper()
	var paths []string
	for _, c := range res["commits"].([]any) {
		paths = append(paths, c.(map[string]any)["file"].(string))
	}
	return paths
}

func settledNotesOf(res map[string]any) []string {
	var out []string
	if ns, ok := res["notes"].([]any); ok {
		for _, n := range ns {
			out = append(out, n.(string))
		}
	}
	return out
}

// settledMessage is the commit message at the tip of branch.
func settledMessage(t *testing.T, ri *repos.RepoInstance, branch string) string {
	t.Helper()
	var msg string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		h, err := svc.HeadCommit(context.Background(), branch)
		require.NoError(t, err)
		info, err := svc.FactsRepo().CommitInfo(context.Background(), plumbing.NewHash(h))
		require.NoError(t, err)
		msg = info.Message
	}))
	return msg
}

func settledNoteFor(i int, hyp, obs string) string {
	return fmt.Sprintf("fact %d: retracted hypothesis %s (settled by this fact, cited in refs); to keep it, re-learn it with distinct_from: [%s].", i, hyp, obs)
}

// SABOTAGE: drop settledNotes from the notes, or settledCommitLine from the
// commit message, or set result["retracted"] to the subsumed paths → red.
func TestLearnHandler_SettledHypothesisIsReported(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]

	res := settledLearn(t, ctx, emb, "settle", nil, settledFact("notes", "a", "Prediction A", "observation"))
	obs := settledWritten(t, res)[0]
	require.NotEqual(t, hyp, obs)

	require.Equal(t, []string{settledNoteFor(0, hyp, obs)}, settledNotesOf(res), "exactly one note, naming the path")
	require.Contains(t, res["summary"], settledNoteFor(0, hyp, obs))
	require.NotContains(t, res["notes"].([]any)[0], "confirmed")
	require.NotContains(t, res["notes"].([]any)[0], "knomit_update", "update cannot restore a deleted fact")
	require.NotContains(t, res, "retracted", "retracted keeps its F04 meaning: the caller's own retract paths")
	require.NotContains(t, res, "commit")
	require.Equal(t, "learn", res["operation"])

	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, 1, strings.Count(msg, "settled:"), msg)
	require.Equal(t, "learn: settle\n\nsettled: "+hyp, strings.TrimSpace(msg))
	require.False(t, factExistsOn(t, ri, "agent/test", hyp))
}

// SABOTAGE: key the notes or the line by call instead of by fact (one note, or
// a line per path) → red.
func TestLearnHandler_SettledBatchGivesTwoNotesAndOneLine(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	h0 := settledWritten(t, settledLearn(t, ctx, emb, "seed0", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]
	h1 := settledWritten(t, settledLearn(t, ctx, emb, "seed1", nil, settledFact("notes", "b", "Prediction B", "hypothesis")))[0]

	res := settledLearn(t, ctx, emb, "settle-both", nil,
		settledFact("notes", "a", "Prediction A", "observation"),
		settledFact("notes", "b", "Prediction B", "observation"))
	w := settledWritten(t, res)
	require.Equal(t, []string{settledNoteFor(0, h0, w[0]), settledNoteFor(1, h1, w[1])}, settledNotesOf(res))
	require.NotContains(t, res, "retracted")

	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, 1, strings.Count(msg, "settled:"), msg)
	require.Contains(t, msg, "\nsettled: "+h0+", "+h1)
}

// No subsumption happens, so no note and no line: distinct_from names the
// hypothesis; learn_dedup is off on the topic; the caller's retract names it
// (reported in `retracted`, not in a settled note).
//
// SABOTAGE: report from the match instead of from the subsumption branch → red.
func TestLearnHandler_NoSettledReportWhenNothingWasSubsumed(t *testing.T) {
	t.Run("distinct_from", func(t *testing.T) {
		ri, ctx, emb := newDedupAttrRepo(t)
		hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]
		res := settledLearn(t, ctx, emb, "keep", nil, settledFact("notes", "a", "Prediction A", "observation", hyp))
		require.Empty(t, settledNotesOf(res))
		require.NotContains(t, settledMessage(t, ri, "agent/test"), "settled:")
		require.True(t, factExistsOn(t, ri, "agent/test", hyp))
	})
	t.Run("learn_dedup off", func(t *testing.T) {
		ri, ctx, emb := newDedupAttrRepo(t)
		hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("inbox", "a", "Prediction A", "hypothesis")))[0]
		res := settledLearn(t, ctx, emb, "off", nil, settledFact("inbox", "a", "Prediction A", "observation"))
		require.Empty(t, settledNotesOf(res))
		require.NotContains(t, settledMessage(t, ri, "agent/test"), "settled:")
		require.True(t, factExistsOn(t, ri, "agent/test", hyp))
	})
	t.Run("caller retract", func(t *testing.T) {
		ri, ctx, emb := newDedupAttrRepo(t)
		hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]
		res := settledLearn(t, ctx, emb, "move", []string{hyp}, settledFact("notes", "a", "Prediction A", "observation"))
		require.Empty(t, settledNotesOf(res))
		require.Equal(t, []any{hyp}, res["retracted"])
		require.NotContains(t, settledMessage(t, ri, "agent/test"), "settled:")
		require.False(t, factExistsOn(t, ri, "agent/test", hyp))
	})
}

// A call that passes `retract` AND triggers subsumption: `retracted` lists only
// the caller's path, the note and the line cover only the subsumed one, and
// both are deleted by the one commit.
//
// SABOTAGE: append the subsumed paths to `retracted` → red.
func TestLearnHandler_RetractAndSubsumptionInOneCall(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed-h", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]
	mine := settledWritten(t, settledLearn(t, ctx, emb, "seed-m", nil, settledFact("notes", "c", "A task of mine", "observation")))[0]

	res := settledLearn(t, ctx, emb, "move-and-settle", []string{mine}, settledFact("notes", "a", "Prediction A", "observation"))
	obs := settledWritten(t, res)[0]
	require.Equal(t, "move", res["operation"])
	require.Equal(t, []any{mine}, res["retracted"], "retracted is the caller's paths only")
	require.Equal(t, []string{settledNoteFor(0, hyp, obs)}, settledNotesOf(res))
	msg := settledMessage(t, ri, "agent/test")
	require.Equal(t, 1, strings.Count(msg, "settled:"), msg)
	require.Contains(t, msg, "\nsettled: "+hyp)
	require.NotContains(t, msg, "settled: "+hyp+", "+mine)
	require.False(t, factExistsOn(t, ri, "agent/test", hyp))
	require.False(t, factExistsOn(t, ri, "agent/test", mine))
}

// The same behaviour inside an experiment: the note, the line, and the delete
// all land on the experiment, and the agent branch is untouched.
//
// SABOTAGE: as TestLearnHandler_SettledHypothesisIsReported → red.
func TestLearnHandler_SettledHypothesisIsReportedInsideAnExperiment(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	hyp := settledWritten(t, settledLearn(t, ctx, emb, "seed", nil, settledFact("notes", "a", "Prediction A", "hypothesis")))[0]
	branch := openTestExperiment(t, ri, "settling")
	agentBefore := headOf(t, ri, "agent/test")
	ectx := repos.WithBinding(ctx, repos.NewBindingOfRepo(ri, branch))

	res := settledLearn(t, ectx, emb, "settle", nil, settledFact("notes", "a", "Prediction A", "observation"))
	obs := settledWritten(t, res)[0]
	require.Equal(t, []string{settledNoteFor(0, hyp, obs)}, settledNotesOf(res))
	require.NotContains(t, res, "retracted")
	msg := settledMessage(t, ri, branch)
	require.Equal(t, 1, strings.Count(msg, "settled:"), msg)
	require.Contains(t, msg, "\nsettled: "+hyp)
	require.False(t, factExistsOn(t, ri, branch, hyp))
	require.True(t, factExistsOn(t, ri, "agent/test", hyp), "the agent branch still has it")
	require.Equal(t, agentBefore, headOf(t, ri, "agent/test"))
}
