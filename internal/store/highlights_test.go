package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// typedFactBody builds a serialized fact with an explicit epistemic type and
// optional local refs (which become DERIVED_FROM edges on upsert).
func typedFactBody(title string, typ fact.Type, conf float64, refs []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = "body"
	f.Kind = fact.Epistemic
	f.Type = typ
	f.Confidence = conf
	f.Sources = 1
	f.Domain = []string{"testing"}
	f.Refs = refs
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// forceCommittedAt overwrites the commit_log row for path (on the branch's
// currently-live commit) to an explicit Unix-second timestamp.
//
// commit_log.committed_at is Unix-SECOND resolution, and sequential
// in-process writes in a test routinely land in the same second — real runs
// showed big and small (two WriteFact calls apart) sharing one timestamp.
// Without this, TestHighlights_RecentAxisOrdersByCommitTime would depend on
// wall-clock luck: a tie falls through to the query's secondary sort key
// (confidence), which could produce the expected order even if the
// committed_at ordering itself were completely broken. Forcing distinct
// values makes the test exercise ONLY the committed_at ORDER BY.
func forceCommittedAt(t *testing.T, svc *Service, branch, path string, ts int64) {
	t.Helper()
	ctx := context.Background()
	branchID, err := svc.rh.branchID(ctx, branch)
	require.NoError(t, err)
	var commitHash string
	err = svc.rh.db.QueryRowContext(ctx,
		`SELECT commit_hash FROM branch_facts WHERE branch_id = ? AND path = ?`,
		branchID, path).Scan(&commitHash)
	require.NoError(t, err)
	res, err := svc.rh.db.ExecContext(ctx,
		`UPDATE commit_log SET committed_at = ? WHERE commit_hash = ? AND path = ?`,
		ts, commitHash, path)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected exactly one commit_log row for %s", path)
}

// seedHighlightFixture writes three observations plus one synthesis deriving
// from all three, one low-impact synthesis deriving from one, and one
// reference (also excluded from highlights, like observations).
//
// big and small's confidences are deliberately DECOUPLED from their commit
// order: big (committed first) has the HIGHER confidence, small (committed
// last) has the LOWER one. This means confidence-as-tiebreak can never
// masquerade as working committed_at ordering on the recent axis — see
// TestHighlights_RecentAxisOrdersByCommitTime. Impact primacy over
// confidence (TestHighlights_RanksByImpactAndExcludesObservations) is
// instead proven by the differing Impact counts alone.
func seedHighlightFixture(t *testing.T, branch string) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	for _, p := range []string{"kb/o/a.md", "kb/o/b.md", "kb/o/c.md"} {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("obs "+p, fact.Observation, 0.5, nil), "add "+p, "")
		require.NoError(t, err)
	}
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/s/big.md",
		typedFactBody("big synthesis", fact.Synthesis, 0.99,
			[]string{"kb/o/a.md", "kb/o/b.md", "kb/o/c.md"}), "add big", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/s/small.md",
		typedFactBody("small synthesis", fact.Synthesis, 0.60,
			[]string{"kb/o/a.md"}), "add small", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/r/ref.md",
		typedFactBody("a reference", fact.Reference, 0.5,
			[]string{"kb/o/a.md"}), "add ref", "")
	require.NoError(t, err)

	// See forceCommittedAt: guarantee small sorts strictly after big on the
	// recent axis without relying on wall-clock/second-granularity luck.
	forceCommittedAt(t, svc, branch, "kb/s/big.md", 1_700_000_000)
	forceCommittedAt(t, svc, branch, "kb/s/small.md", 1_700_000_100)

	return svc
}

func TestHighlights_RanksByImpactAndExcludesObservations(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)

	require.Len(t, res.Highlights, 2, "observations and references must be excluded")
	require.Equal(t, "kb/s/big.md", res.Highlights[0].Path)
	require.Equal(t, 3, res.Highlights[0].Impact)
	require.Equal(t, "kb/s/small.md", res.Highlights[1].Path)
	require.Equal(t, 1, res.Highlights[1].Impact)
	for _, h := range res.Highlights {
		require.NotEqual(t, "kb/r/ref.md", h.Path, "references must be excluded")
		require.NotEqual(t, "reference", h.Type, "references must be excluded")
	}

	// Impact primacy is proven by the Impact counts above (3 vs 1), not by
	// confidence: big now carries the higher confidence too, since recency
	// needs it decoupled from commit order elsewhere (seedHighlightFixture).
	require.Greater(t, res.Highlights[0].Confidence, res.Highlights[1].Confidence)
}

func TestHighlights_TypesMapCountsLiveFactsByType(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)

	require.Equal(t, 3, res.Types["observation"])
	require.Equal(t, 2, res.Types["synthesis"])
	require.Equal(t, 1, res.Types["reference"])
}

func TestHighlights_ImpactIsGlobalNotPathScoped(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)
	ctx := context.Background()

	rootRes, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	scopedRes, err := svc.FactQuery().Stats(ctx, branch, "kb/s/", "")
	require.NoError(t, err)

	require.Equal(t, "kb/s/big.md", rootRes.Highlights[0].Path)
	require.Equal(t, "kb/s/big.md", scopedRes.Highlights[0].Path)
	require.Equal(t, rootRes.Highlights[0].Impact, scopedRes.Highlights[0].Impact,
		"impact must not change with pathPrefix")
}

func TestHighlights_PathPrefixScopesTheList(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	// Scoping is proven by WHICH facts come back, not by an empty list: kb/o/
	// used to return nothing here, but that was the type exclusion emptying it,
	// not the path scope, and the two failure modes looked identical.
	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/s/", "")
	require.NoError(t, err)
	require.NotEmpty(t, res.Highlights)
	for _, h := range res.Highlights {
		require.True(t, strings.HasPrefix(h.Path, "kb/s/"), "out of scope: %s", h.Path)
	}

	res, err = svc.FactQuery().Stats(context.Background(), branch, "kb/o/", "")
	require.NoError(t, err)
	require.NotEmpty(t, res.Highlights)
	for _, h := range res.Highlights {
		require.True(t, strings.HasPrefix(h.Path, "kb/o/"), "out of scope: %s", h.Path)
	}
}

// TestHighlights_OnlyTheRequestedBranch is the load-bearing test for this
// feature. The graph is TEMPORAL: it holds one node per fact VERSION, and
// those nodes are not branch-scoped. Two branches carrying different content
// at the same path therefore have two nodes, each with its own edges. Only the
// join through branch_facts keeps them apart.
//
// This is the mistake the design went through twice: reading node_props_text
// directly surfaced three versions of one path in a single top-10, plus
// near-duplicate facts a prior review had already subsumed.
func TestHighlights_OnlyTheRequestedBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	// Shared substrate on main.
	for _, p := range []string{"kb/o/a.md", "kb/o/b.md"} {
		_, err := svc.Facts().WriteFact(ctx, "main", p,
			typedFactBody("obs "+p, fact.Observation, 0.5, nil), "add "+p, "")
		require.NoError(t, err)
	}
	// main's version of the contested path: derives from both.
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/s/contested.md",
		typedFactBody("main version", fact.Synthesis, 0.5,
			[]string{"kb/o/a.md", "kb/o/b.md"}), "main version", "")
	require.NoError(t, err)

	// A second branch off main, with a DIFFERENT version at the same path
	// deriving from only one observation.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "other", "main"))
	_, err = svc.Facts().WriteFact(ctx, "other", "kb/s/contested.md",
		typedFactBody("other version", fact.Synthesis, 0.5,
			[]string{"kb/o/a.md"}), "other version", "")
	require.NoError(t, err)

	mainRes, err := svc.FactQuery().Stats(ctx, "main", "", "")
	require.NoError(t, err)
	otherRes, err := svc.FactQuery().Stats(ctx, "other", "", "")
	require.NoError(t, err)

	// Each branch sees exactly one version of the contested path...
	require.Len(t, mainRes.Highlights, 1)
	require.Len(t, otherRes.Highlights, 1)
	require.Equal(t, "main version", mainRes.Highlights[0].Title)
	require.Equal(t, "other version", otherRes.Highlights[0].Title)

	// ...with ITS OWN edge count, not the other branch's and not the sum.
	require.Equal(t, 2, mainRes.Highlights[0].Impact)
	require.Equal(t, 1, otherRes.Highlights[0].Impact)
}

// TestHighlights_RecentAxisOrdersByCommitTime: the axis must reach the ORDER
// BY, not just be echoed back. small is written after big.
func TestHighlights_RecentAxisOrdersByCommitTime(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	byImpact, err := svc.FactQuery().Stats(context.Background(), branch, "", "impact")
	require.NoError(t, err)
	require.Equal(t, "kb/s/big.md", byImpact.Highlights[0].Path)

	byRecent, err := svc.FactQuery().Stats(context.Background(), branch, "", "recent")
	require.NoError(t, err)
	require.Equal(t, "kb/s/small.md", byRecent.Highlights[0].Path,
		"small was committed last")

	// default_axis is the recommendation and must NOT follow the request.
	require.Equal(t, byImpact.DefaultAxis, byRecent.DefaultAxis)
}

// TestHighlights_UnknownAxisFallsBackToDefault.
func TestHighlights_UnknownAxisFallsBackToDefault(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	bogus, err := svc.FactQuery().Stats(context.Background(), branch, "", "nonsense")
	require.NoError(t, err)
	require.Equal(t, "kb/s/big.md", bogus.Highlights[0].Path)
}

// TestHighlights_ImpactCountsDistinctTargetsNotEdgeRows is the regression
// test for the impact-inflation bug (2026-08-04 overview-highlights review,
// finding C1): graphAddDerivedFromAtCommitTx (derived_from.go) writes one
// DERIVED_FROM edge per ref PER source_commit — graphDerivedFromEdgeExists
// dedups on (src, tgt, source_commit, target_commit), deliberately, because
// edges are immutable lineage assertions at a commit. When the SAME blob is
// re-indexed at a second commit (unchanged content — e.g. an unrelated later
// write that leaves this path's blob hash untouched but re-asserts its refs
// during sync), its lineage is recorded a SECOND time: a fresh edge row per
// ref, sharing the same graph node (same path, same blob hash) and the same
// targets. Impact must count DISTINCT target facts, not edge rows, so a fact
// with N refs reports N impact regardless of how many times its lineage was
// independently asserted — measured against the production `core` KB, 176 of
// 216 edge-carrying live facts were inflated this way (81%), max ratio 3.0x.
func TestHighlights_ImpactCountsDistinctTargetsNotEdgeRows(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	refPaths := []string{"kb/o/a.md", "kb/o/b.md", "kb/o/c.md"}
	refBlobs := make(map[string]string, len(refPaths))
	for _, p := range refPaths {
		res, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("obs "+p, fact.Observation, 0.5, nil), "add "+p, "")
		require.NoError(t, err)
		refBlobs[p] = res.BlobHash
	}
	srcRes, err := svc.Facts().WriteFact(ctx, branch, "kb/s/big.md",
		typedFactBody("big synthesis", fact.Synthesis, 0.9, refPaths), "add big", "")
	require.NoError(t, err)

	res, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	require.Len(t, res.Highlights, 1)
	require.Equal(t, 3, res.Highlights[0].Impact,
		"sanity: 3 refs -> impact 3 before the duplicate lineage is added")

	// Simulate the same lineage being asserted a SECOND time at a distinct
	// source_commit, mirroring graphAddDerivedFromAtCommitTx's real dedup key
	// (src, tgt, source_commit, target_commit). Insert edges directly via the
	// graph primitives — the same pattern
	// TestGraphSetEdgeProps_WritesAndReadsText (edge_props_test.go) uses.
	si := svc.si
	srcID, err := si.graphNodeIDByBlob(ctx, "kb/s/big.md", srcRes.BlobHash)
	require.NoError(t, err)
	require.NotZero(t, srcID)
	for _, p := range refPaths {
		tgtID, err := si.graphNodeIDByBlob(ctx, p, refBlobs[p])
		require.NoError(t, err)
		require.NotZero(t, tgtID)
		edgeID, err := si.graphInsertEdgeReturningID(ctx, srcID, tgtID, EdgeDerivedFrom)
		require.NoError(t, err)
		require.NoError(t, si.graphSetEdgeProps(ctx, edgeID, map[string]string{
			"source_commit": "fake-second-source-commit",
			"target_commit": "fake-second-target-commit",
		}))
	}

	res2, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	require.Len(t, res2.Highlights, 1)
	require.Equal(t, 3, res2.Highlights[0].Impact,
		"impact must count distinct target facts, not edge rows: "+
			"3 refs asserted twice (6 edge rows total) must still read 3, not 6")
}

func TestHighlights_EmptyIsSliceNotNil(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)
	require.NotNil(t, res.Highlights)
	require.NotNil(t, res.Types)
}

// A scope whose ONLY facts are excluded types showed an empty Highlights
// section — the panel's whole reason for existing, blank, on a folder that
// plainly has content. The exclusion exists to stop 1,186 observations burying
// 128 syntheses; where there is no distilled layer to bury, it protects nothing
// and only removes the section.
func TestHighlights_ExcludedOnlyScopeFallsBackToWhatItHas(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	// kb/o holds three observations and nothing else.
	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/o", "")
	require.NoError(t, err)

	require.Len(t, res.Highlights, 3, "a pure-observation scope must still have highlights")
	for _, h := range res.Highlights {
		require.Equal(t, "observation", h.Type)
	}
}

func TestHighlights_FallbackDoesNotFireWhereAnyEligibleFactExists(t *testing.T) {
	// The guard that keeps the fallback from quietly repealing the exclusion:
	// one synthesis in scope is enough for the excluded types to stay out, even
	// though observations outnumber it three to one.
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)

	require.Len(t, res.Highlights, 2)
	for _, h := range res.Highlights {
		require.NotEqual(t, "observation", h.Type)
		require.NotEqual(t, "reference", h.Type)
	}
}

func TestHighlights_ExcludedOnlyScopeStillHonoursTheAxis(t *testing.T) {
	// The fallback reuses the same ORDER BY, so a fallback list is ranked, not
	// whatever order the rows happened to come back in.
	const branch = "main"
	svc := seedHighlightFixture(t, branch)
	forceCommittedAt(t, svc, branch, "kb/o/b.md", 1_800_000_000)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/o", AxisRecent)
	require.NoError(t, err)

	require.Len(t, res.Highlights, 3)
	require.Equal(t, "kb/o/b.md", res.Highlights[0].Path, "most recent first on the recent axis")
}

// The fallback has to be VISIBLE to callers, not just correct. "Is there a
// distilled layer to bury here" is a question about a scope, and a lens union
// is a scope no single mount can see: handleHALLensStats merges every mount's
// top-N, so it needs to know which of those lists are excluded types standing
// in for an empty one.
func TestHighlights_StatsReportsWhetherTheFallbackFired(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/o", "")
	require.NoError(t, err)
	require.NotEmpty(t, res.Highlights)
	require.True(t, res.HighlightsFallback, "a pure-observation scope answered with excluded types")

	res, err = svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)
	require.NotEmpty(t, res.Highlights)
	require.False(t, res.HighlightsFallback, "the root has eligible facts, so nothing fell back")
}

func TestHighlights_AnEmptyScopeIsNotAFallback(t *testing.T) {
	// Nothing to fall back TO. Reporting a fallback here would let an empty
	// mount look like one holding excluded types, and a union consumer would
	// then treat its (absent) list as something to suppress.
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/nothing-here", "")
	require.NoError(t, err)
	require.Empty(t, res.Highlights)
	require.False(t, res.HighlightsFallback)
}
