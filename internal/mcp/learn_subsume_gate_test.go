package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// SUBSUMPTION MUST SURVIVE THE REF GATE.
//
// subsumeHypothesis adds the hypothesis path to BOTH the observation's refs and
// the call's retract list. A gate that rejects a ref to a same-call retraction
// therefore made the entire observation-settles-a-prediction workflow
// unwritable — knomit_learn answered "kb/…/hyp.md ... does not exist" for a
// fact that existed and was being retracted by that very call.
//
// The citation is lineage: the retracted fact keeps a valid version in history
// and every anchored read walks back through retractions, so the ref still
// resolves for anyone who follows it.
func TestLearnHandler_SubsumedHypothesisSurvivesRefGate(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	hyp := map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Prediction that memory portability will hold",
		"body":  "Hypothesis statement; evidence chain; reasoning; gaps; falsification.",
		"type":  "hypothesis", "confidence": 0.5, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
	}
	rh, err := LearnHandler(emb)(ctx, learnReq("seed-hyp", hyp))
	require.NoError(t, err)
	require.False(t, rh.IsError, "hypothesis seed failed: %s", resultText(t, rh))
	hypPath := mergedFactPath(t, rh)

	// Identical title+body → identical stub vector → dedup match, so the
	// observation subsumes the hypothesis rather than landing beside it.
	obs := map[string]any{}
	for k, v := range hyp {
		obs[k] = v
	}
	obs["type"] = "observation"
	obs["confidence"] = 0.9
	obs["sources"] = 2

	ro, err := LearnHandler(emb)(ctx, learnReq("settle-hyp", obs))
	require.NoError(t, err)
	require.False(t, ro.IsError,
		"an observation subsuming a hypothesis must be writable: %s", resultText(t, ro))

	// The observation landed at its OWN path, citing the hypothesis it settled.
	obsPath := mergedFactPath(t, ro)
	require.NotEqual(t, hypPath, obsPath,
		"the observation is a new fact, not a revision of the prediction it settles")

	written := readBack(t, svc, obsPath)
	require.Contains(t, written.Refs, fact.QualifyKBPath(repoID12(t, svc), hypPath),
		"the subsumed hypothesis must be cited as lineage, in canonical stored form")

	// And the hypothesis really is gone from the branch — the retract landed in
	// the same commit, which is what made the citation look dangling.
	exists, err := svc.Facts().FactExists(ctx, "agent/test", hypPath)
	require.NoError(t, err)
	require.False(t, exists, "the subsumed hypothesis must be retracted")
}

// repoID12 is the wire-form id of the test repo, for asserting on canonical refs.
func repoID12(t *testing.T, svc *store.Service) string {
	t.Helper()
	root, err := svc.RootCommit(context.Background(), "agent/test")
	require.NoError(t, err)
	return fact.ID12(root)
}
