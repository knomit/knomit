package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAbstraction_TitleVectorLifecycle covers the whole of what makes the axis
// watermark-incremental: coverage counts the live epistemic set, a stored
// vector reads back, and EDITING a fact drops coverage because facts rows are
// content-addressed — the edited fact is a new row with no vector, which is
// exactly the delta a review session has to embed.
func TestAbstraction_TitleVectorLifecycle(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	ax := svc.Abstraction()

	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")

	have, total, err := ax.TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 0, have)
	require.Equal(t, 1, total, "one live epistemic fact, no title vector yet")

	targets, err := ax.LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "kb/a.md", targets[0].Path)
	require.Equal(t, "Alpha", targets[0].Title)

	require.NoError(t, ax.PutTitleVectors(ctx, []TitleVector{{
		FactID: targets[0].FactID,
		Path:   targets[0].Path,
		Vec:    unitVectorAt(0, 768),
	}}))

	have, total, err = ax.TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 1, have)
	require.Equal(t, 1, total)

	targets, err = ax.LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Empty(t, targets, "a fact with a vector is not a backfill target")

	writeTestFact(t, svc, "kb/a.md", "Alpha revised", "body-a-2")

	have, total, err = ax.TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 0, have, "an edited fact is a new row, so the axis is stale by construction")
	require.Equal(t, 1, total)
}

// TestAbstraction_PragmaticFactsAreNotOnTheAxis is a correctness filter, not a
// preference: prune's decision path does not carry Kind through mergedFact, so
// a pragmatic fact reaching synthesis would be silently rewritten as epistemic
// (the reason reviewStrategy.AcceptSeed exists). A shortlist that offered one
// would corrupt it, so pragmatic facts never join the axis.
func TestAbstraction_PragmaticFactsAreNotOnTheAxis(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)

	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/policy.md",
		"---\nkind: pragmatic\ntype: policy\n---\n# Policy\n\nbody", "add policy", "test")
	require.NoError(t, err)

	_, total, err := svc.Abstraction().TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 0, total)

	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Empty(t, targets)
}

// TestAbstraction_TopTitleNeighbours proves the KNN over-fetch does its job:
// the vec0 k window is applied BEFORE the branch/kind filter, so a naive k
// would return short (or empty) once historical or pragmatic rows crowd the
// window. Here five epistemic facts share the axis with a pragmatic one and a
// superseded revision, and the query still returns live epistemic neighbours
// only, self excluded, in descending similarity.
func TestAbstraction_TopTitleNeighbours(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	ax := svc.Abstraction()

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeTestFact(t, svc, "kb/"+name+".md", "Title "+name, "body-"+name)
	}
	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/pragmatic.md",
		"---\nkind: pragmatic\ntype: policy\n---\n# Pragmatic\n\nbody", "add", "test")
	require.NoError(t, err)

	targets, err := ax.LiveFactsMissingTitleVector(ctx, "agent/test", 100)
	require.NoError(t, err)
	require.Len(t, targets, 5)

	// Place the vectors on a ring so similarity order is known: index i sits at
	// angle i, so neighbours of 0 are 1, then 2, then 3...
	for i, tgt := range targets {
		require.NoError(t, ax.PutTitleVectors(ctx, []TitleVector{{
			FactID: tgt.FactID, Path: tgt.Path, Vec: ringVector(i, len(targets), 768),
		}}))
	}

	got, err := ax.TopTitleNeighbours(ctx, "agent/test", targets[0].FactID, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, n := range got {
		require.NotEqual(t, targets[0].FactID, n.FactID, "self must be excluded")
		require.NotEqual(t, "kb/pragmatic.md", n.Path)
	}
	require.GreaterOrEqual(t, got[0].Similarity, got[1].Similarity, "descending similarity")
}

// TestAbstraction_TopTitleNeighboursForUnembeddedFact returns nothing rather
// than erroring: during a partial backfill most facts have no vector yet, and
// the shortlist has to tolerate that quietly.
func TestAbstraction_TopTitleNeighboursForUnembeddedFact(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")

	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	got, err := svc.Abstraction().TopTitleNeighbours(ctx, "agent/test", targets[0].FactID, 5)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestAbstraction_BodyVectorsByFactID reads the EXISTING facts_vec rows. The
// shortlist needs blended cosines and must never re-embed to get them
// (conventions/synthesize/scoped-cluster-queryby-path).
func TestAbstraction_BodyVectorsByFactID(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")
	writeTestFact(t, svc, "kb/b.md", "Beta", "body-b")

	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	ids := []int64{targets[0].FactID, targets[1].FactID}

	vecs, err := svc.Abstraction().BodyVectorsByFactID(ctx, ids)
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	require.Len(t, vecs[ids[0]], 768)
}

// TestAbstraction_EmbedIdentityChangeClearsTheAxis — title vectors are model
// vectors. When the embedding identity drifts, facts_vec is recreated empty and
// the axis (and the caches derived from it) must go with it, or the next
// session would compare cosines from two different models.
func TestAbstraction_EmbedIdentityChangeClearsTheAxis(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "k.db")

	svc := openAbstractionTestServiceAt(t, path, &stub768Embedder{})
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")
	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.NoError(t, svc.Abstraction().PutTitleVectors(ctx, []TitleVector{{
		FactID: targets[0].FactID, Path: targets[0].Path, Vec: unitVectorAt(0, 768),
	}}))
	require.NoError(t, svc.Close())

	// Reopen under a DIFFERENT model id and run the startup heal the repos
	// layer runs: NeedsRebuild sees the embedding identity drift, and Rebuild
	// recreates facts_vec empty. The axis must go the same way.
	svc2 := openAbstractionTestServiceAt(t, path, &otherIDEmbedder{})
	needs, err := svc2.IndexManager().NeedsRebuild(ctx, "agent/test")
	require.NoError(t, err)
	require.True(t, needs, "an embedding-identity change must demand a rebuild")
	require.NoError(t, svc2.IndexManager().Rebuild(ctx, "agent/test", nil))

	have, total, err := svc2.Abstraction().TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 1, total, "the fact is still there")
	require.Equal(t, 0, have, "vectors from another model must not survive")
}
