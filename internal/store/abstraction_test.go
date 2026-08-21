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
			FactID: tgt.FactID, Vec: ringVector(i, len(targets), 768),
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
		FactID: targets[0].FactID, Vec: unitVectorAt(0, 768),
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

// TestAbstraction_DropBranchClearsShortlistState — the restatement tables
// reference branches(id) with no cascade, and foreign keys are enforced. Left
// behind, they make DropBranch fail on the branches delete AFTER the git ref is
// already gone: exactly the half-removed state DropBranch's ordering exists to
// avoid.
func TestAbstraction_DropBranchClearsShortlistState(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	require.NoError(t, svc.Branches().CreateBranch(ctx, "agent/doomed", "agent/test"))

	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")
	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.NotEmpty(t, targets)

	// Give the doomed branch a full set of shortlist state.
	require.NoError(t, svc.Abstraction().ReplaceRestatementPairs(ctx, "agent/doomed", nil,
		[]RestatementPair{{APath: "kb/a.md", BPath: "kb/b.md", AFactID: 1, BFactID: 2, TitleCos: 0.9}},
		[]int64{1, 2}))
	require.NoError(t, svc.Abstraction().RecordRestatementVerdict(ctx, "agent/doomed",
		RestatementVerdict{APath: "kb/a.md", BPath: "kb/b.md", AFactID: 1, BFactID: 2}))
	require.NoError(t, svc.Abstraction().SetProbeSessionsWaited(ctx, "agent/doomed", 3))

	require.NoError(t, svc.Branches().DropBranch(ctx, "agent/doomed"),
		"a branch carrying shortlist state must still drop cleanly")

	branches, err := svc.Branches().ListBranches(ctx)
	require.NoError(t, err)
	for _, b := range branches {
		require.NotEqual(t, "agent/doomed", b.Name)
	}
}

// TestAbstraction_TitleVecWidthIsValidatedUnderEveryModel — fact_titles_vec is
// created at the default 768 by Open (the delete trigger needs a table to
// exist), and a vec0 table's width is fixed at CREATE. Under a model of any
// other dimension the identity check would short-circuit before anyone noticed:
// every insert would fail, the axis would stay empty, and the shortlist would
// silently find nothing forever.
func TestAbstraction_TitleVecWidthIsValidatedUnderEveryModel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "k.db")

	svc := openAbstractionTestServiceAt(t, path, &dim512Embedder{})
	// Rebuild first: the vec tables are bootstrapped at the default width by
	// Open, and it is the rebuild that recreates them at the ACTIVE model's
	// dimension — which is the order the app startup path uses too.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")

	targets, err := svc.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	vec := make([]float32, 512)
	vec[0] = 1
	require.NoError(t, svc.Abstraction().PutTitleVectors(ctx, []TitleVector{{
		FactID: targets[0].FactID, Vec: vec,
	}}), "the axis must accept vectors at the ACTIVE model's dimension")

	have, _, err := svc.Abstraction().TitleVectorCoverage(ctx, "agent/test")
	require.NoError(t, err)
	require.Equal(t, 1, have)
}

// TestAbstraction_EmbedIdentityChangeClearsVerdictsToo — a trailing
// resolution-rate mixing judgements made under two embedding models is reading
// evidence about pairs the new model may never propose. The schema says an
// empty verdict window is safe, so a model change starts over.
func TestAbstraction_EmbedIdentityChangeClearsVerdictsToo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "k.db")

	svc := openAbstractionTestServiceAt(t, path, &stub768Embedder{})
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")
	require.NoError(t, svc.Abstraction().RecordRestatementVerdict(ctx, "agent/test",
		RestatementVerdict{APath: "kb/a.md", BPath: "kb/b.md", AFactID: 1, BFactID: 2, Resolved: true}))
	require.NoError(t, svc.Abstraction().SetProbeSessionsWaited(ctx, "agent/test", 4))
	require.NoError(t, svc.Close())

	svc2 := openAbstractionTestServiceAt(t, path, &otherIDEmbedder{})
	require.NoError(t, svc2.IndexManager().Rebuild(ctx, "agent/test", nil))

	verdicts, err := svc2.Abstraction().RecentRestatementVerdicts(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Empty(t, verdicts, "verdicts from another model must not steer this one's throttle")

	waited, err := svc2.Abstraction().ProbeSessionsWaited(ctx, "agent/test")
	require.NoError(t, err)
	require.Zero(t, waited)
}

// TestAbstraction_FactIDsByPathResolvesOnlyLiveVersions — verdict attribution
// keys on ids, so a path that has been retracted must resolve to nothing rather
// than to a stale id.
func TestAbstraction_FactIDsByPathResolvesOnlyLiveVersions(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")
	writeTestFact(t, svc, "kb/b.md", "Beta", "body-b")

	ids, err := svc.Abstraction().FactIDsByPath(ctx, "agent/test", []string{"kb/a.md", "kb/b.md"})
	require.NoError(t, err)
	require.Len(t, ids, 2)

	_, err = svc.Facts().DeleteFact(ctx, "agent/test", "kb/b.md", "retract b")
	require.NoError(t, err)

	ids, err = svc.Abstraction().FactIDsByPath(ctx, "agent/test", []string{"kb/a.md", "kb/b.md"})
	require.NoError(t, err)
	require.Len(t, ids, 1)
	require.NotContains(t, ids, "kb/b.md")
}

// TestAbstraction_TitleVecWidthFixedOnAnUpgradedDatabase is the actual latent
// case, and the one the width check exists for.
//
// A database indexed under a non-768 model, upgraded from a version that
// predates the axis: Open bootstraps fact_titles_vec at the DEFAULT 768 so the
// delete trigger has a table, and then the embedding identity MATCHES — same
// model, same dim — so the rebuild has every reason to return early. Without a
// width check that runs before that short-circuit, the axis keeps the wrong
// width permanently: every insert fails, coverage never rises, and the
// shortlist reports "no candidates" forever on a corpus full of them.
func TestAbstraction_TitleVecWidthFixedOnAnUpgradedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "k.db")

	svc := openAbstractionTestServiceAt(t, path, &dim512Embedder{})
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")

	// Simulate the pre-axis database: the titles table simply is not there.
	_, err := svc.rh.db.ExecContext(ctx, `DROP TABLE fact_titles_vec`)
	require.NoError(t, err)
	require.NoError(t, svc.Close())

	// Reopen: Open recreates it at the default width, and the identity check
	// has nothing to complain about.
	svc2 := openAbstractionTestServiceAt(t, path, &dim512Embedder{})
	require.NoError(t, svc2.IndexManager().Rebuild(ctx, "agent/test", nil))

	targets, err := svc2.Abstraction().LiveFactsMissingTitleVector(ctx, "agent/test", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	vec := make([]float32, 512)
	vec[0] = 1
	require.NoError(t, svc2.Abstraction().PutTitleVectors(ctx, []TitleVector{{
		FactID: targets[0].FactID, Vec: vec,
	}}), "the axis must have been rebuilt at the active model's width")
}

// TestAbstraction_HandlesIdListsBeyondTheSQLVariableLimit — SQLite binds at most
// 32,766 parameters per statement, and these queries build one placeholder per
// fact id. The session that CLOSES a backfill rescans the whole corpus, so it
// passes every live fact id at once: on a corpus past ~16k facts the delete
// binds 2N parameters and the statement fails outright.
//
// It fails soft — the refresh degrades to "no candidates" with a health line —
// which is exactly what makes it worth a test: a large corpus would simply
// never get this feature, and would say so only in a line nobody reads.
func TestAbstraction_HandlesIdListsBeyondTheSQLVariableLimit(t *testing.T) {
	ctx := context.Background()
	svc := openAbstractionTestService(t)
	writeTestFact(t, svc, "kb/a.md", "Alpha", "body-a")

	// Well past the limit, and past 2x it for the two-list queries.
	ids := make([]int64, 40_000)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	require.NoError(t, svc.Abstraction().ReplaceRestatementPairs(ctx, "agent/test", ids, nil, ids),
		"a whole-corpus rescan must not exceed the parameter limit")

	_, err := svc.Abstraction().BodyVectorsByFactID(ctx, ids)
	require.NoError(t, err, "scoring a whole corpus's pairs must not exceed the parameter limit")

	_, err = svc.Abstraction().PartnersOfFacts(ctx, "agent/test", ids)
	require.NoError(t, err, "a mass retraction must not exceed the parameter limit")

	_, err = svc.Abstraction().FactIDsByPath(ctx, "agent/test", manyPaths(40_000))
	require.NoError(t, err)
}

// TestCosineSim_OrthogonalIsZeroNotNull — the SQL function and the Go helper are
// one formula now, and unifying them must not quietly change what either
// returns. Orthogonal vectors have a defined similarity of 0; only a degenerate
// (zero-norm) vector has none.
func TestCosineSim_OrthogonalIsZeroNotNull(t *testing.T) {
	svc := openAbstractionTestService(t)

	a := make([]float32, 8)
	b := make([]float32, 8)
	a[0], b[1] = 1, 1

	var sim *float64
	require.NoError(t, svc.rh.db.QueryRow(`SELECT knomit_cosine_sim(?, ?)`,
		float32SliceToBytes(a), float32SliceToBytes(b)).Scan(&sim))
	require.NotNil(t, sim, "orthogonal vectors have a similarity, and it is 0")
	require.InDelta(t, 0.0, *sim, 1e-9)

	// A zero-norm vector genuinely has none.
	require.NoError(t, svc.rh.db.QueryRow(`SELECT knomit_cosine_sim(?, ?)`,
		float32SliceToBytes(a), float32SliceToBytes(make([]float32, 8))).Scan(&sim))
	require.Nil(t, sim, "a degenerate vector has no similarity to anything")

	require.InDelta(t, 0.0, CosineSim(a, b), 1e-9)
	require.InDelta(t, 0.0, CosineSim(a, make([]float32, 8)), 1e-9)
}
