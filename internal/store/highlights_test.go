package store

import (
	"context"
	"path/filepath"
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

// seedHighlightFixture writes three observations plus one synthesis deriving
// from all three, and one low-impact synthesis deriving from one.
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
		typedFactBody("big synthesis", fact.Synthesis, 0.60,
			[]string{"kb/o/a.md", "kb/o/b.md", "kb/o/c.md"}), "add big", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/s/small.md",
		typedFactBody("small synthesis", fact.Synthesis, 0.99,
			[]string{"kb/o/a.md"}), "add small", "")
	require.NoError(t, err)
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

	// Impact ranking must beat confidence: small has .99, big has .60.
	require.Greater(t, res.Highlights[1].Confidence, res.Highlights[0].Confidence)
}

func TestHighlights_TypesMapCountsLiveFactsByType(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)

	require.Equal(t, 3, res.Types["observation"])
	require.Equal(t, 2, res.Types["synthesis"])
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

	res, err := svc.FactQuery().Stats(context.Background(), branch, "kb/o/", "")
	require.NoError(t, err)
	require.Empty(t, res.Highlights, "kb/o/ holds only observations")
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
