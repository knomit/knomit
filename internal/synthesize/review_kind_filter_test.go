package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestDirtyFacts_ExcludesPragmaticFacts is a regression test for the synthesis
// pipeline silently rewriting pragmatic facts as epistemic. The merge and
// distill paths in decision.go construct output facts without propagating
// Kind, so any pragmatic fact reaching synthesis would be written back with
// Kind defaulted to Epistemic and its original deleted. By keeping pragmatic
// facts out of the candidate set, the synthesis pipeline (which is designed
// to operate on descriptive knowledge) cannot rewrite a policy/heuristic.
//
// Both code paths in dirtyFacts are exercised: the index-Search path used on
// the first run (no watermark) and the DiffFiles incremental path used after
// the watermark is set.
func TestDirtyFacts_ExcludesPragmaticFacts(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	const epPath = "kb/technology/obs.md"
	const pragPath = "kb/technology/pol.md"

	t.Run("first run (index path)", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		writeKindFact(t, svc, branch, pragPath, fact.Pragmatic, fact.Policy)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		seeds, err := r.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		paths := seedPaths(seeds)
		require.Contains(t, paths, epPath, "epistemic fact must be selected")
		require.NotContains(t, paths, pragPath, "pragmatic fact must be excluded from synthesis")
	})

	t.Run("incremental (diff path)", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		// Seed an unrelated baseline fact, then anchor the watermark at HEAD
		// so the two facts written next are visible as "changed since
		// watermark" via DiffFiles.
		writeKindFact(t, svc, branch, "kb/technology/baseline.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))

		writeKindFact(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		writeKindFact(t, svc, branch, pragPath, fact.Pragmatic, fact.Policy)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		seeds, err := r.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		paths := seedPaths(seeds)
		require.Contains(t, paths, epPath, "epistemic fact must be selected")
		require.NotContains(t, paths, pragPath, "pragmatic fact must be excluded from synthesis")
	})
}

func writeKindFact(t *testing.T, svc *store.Service, branch, path string, kind fact.Kind, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body for " + path
	f.Kind = kind
	f.Type = typ
	f.Confidence = 0.7
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, path, content, "seed-"+string(kind), "")
	require.NoError(t, err)
}

func seedPaths(seeds []factForLLM) []string {
	out := make([]string, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, s.File)
	}
	return out
}
