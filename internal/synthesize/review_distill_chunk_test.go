package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestReviewer_DistillItemsAreChunked pins the transport bound on distill work
// items. Before chunkFacts was wired, a first-run session marshalled the ENTIRE
// seed corpus — bounded only by dirtyFacts' Limit: 100_000 full scan — into a
// single "distill-all" item, i.e. one prompt containing the whole knowledge
// base.
//
// The corpus here is sized past 2× maxDistillChunkBytes so a correct
// implementation must split it, and the assertions are on the persisted work
// items rather than on chunkFacts directly: the defect was never in chunkFacts
// (which was correct and unused), it was in StartSession not calling it.
func TestReviewer_DistillItemsAreChunked(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	branch := "agent/test"
	ctx := context.Background()

	// 24 facts × ~12 KiB of body ≈ 288 KiB of fact JSON, comfortably past
	// 2 × maxDistillChunkBytes (128 KiB) so at least three chunks are required.
	// Each individual fact stays far below the budget, so every chunk must
	// honour the bound (chunkFacts only overshoots for a single oversized fact).
	const (
		numSeeds = 24
		bodySize = 12 * 1024
	)
	body := strings.Repeat("x", bodySize)
	for i := 0; i < numSeeds; i++ {
		f := fact.NewFact(fmt.Sprintf("kb/test/seed-%02d.md", i))
		f.Title = fmt.Sprintf("seed %02d", i)
		f.Body = body
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		serialized, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, f.Path(), serialized, "seed", "")
		require.NoErrorf(t, err, "seed %d", i)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewer(ri, nil)
	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	// Drain the queue through the store rather than ContinueSession: answering
	// via the pipeline index records the item without applying its decisions,
	// so no RAPTOR follow-ups land mid-walk and confuse the census.
	var distillKeys []string
	for steps := 0; steps < 200; steps++ {
		item, err := svc.Pipeline().NextPipelineWorkItem(ctx, res.SessionID)
		require.NoError(t, err)
		if item == nil {
			break
		}
		if item.StepType == "distill" {
			require.LessOrEqual(t, len(item.FactsJSON), maxDistillChunkBytes,
				"distill item %q payload exceeds maxDistillChunkBytes", item.ClusterKey)

			// The payload must still be a well-formed fact list — chunking splits
			// between facts, never inside one.
			var facts []factForLLM
			require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &facts),
				"distill item %q payload is not a fact array", item.ClusterKey)
			require.NotEmpty(t, facts, "distill item %q is empty", item.ClusterKey)

			distillKeys = append(distillKeys, item.ClusterKey)
		}
		claimed, err := svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, "{}")
		require.NoError(t, err)
		require.True(t, claimed)
	}

	require.GreaterOrEqual(t, len(distillKeys), 2,
		"a corpus past 2× the chunk budget must yield ≥2 distill items, got %v", distillKeys)
	for i, key := range distillKeys {
		require.Equal(t, fmt.Sprintf("distill-all-%d", i), key,
			"distill chunks must be keyed and served in insertion order")
	}
}
