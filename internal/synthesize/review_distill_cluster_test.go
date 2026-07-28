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

// Depth-0 distill grouping.
//
// Depth-0 distill did not use clusters at all: Plan passed the whole flat seed
// pool to chunkFacts, so an item was an arbitrary byte slice of the corpus in
// scan order (ClusterKey "distill-all-N"). Prune (one item per Louvain
// community) and the RAPTOR follow-ups (one item per re-clustered community)
// both worked on real clusters; depth-0 was the odd one out.
//
// Grouping by cluster is what makes a smaller item stop being a quality loss:
// the bound comes from the corpus's own structure rather than from a byte
// count. But two properties of ScopedCluster make the naive "iterate clusters"
// version wrong, and both tests below exist to pin them:
//
//   - filterSmallClusters DROPS any community below minCommunitySize, so a seed
//     that clusters alone would silently vanish from synthesis.
//   - clusters contain NEIGHBOURS, not just seeds — ScopedCluster pulls the top
//     10 search hits per seed into the subgraph, and the review path passes no
//     ExcludeTypes, so a cluster can hold facts AcceptSeed deliberately refuses
//     (pragmatic ones, which decision.go would rewrite as epistemic on commit).
//
// Silent loss on a synthesis path is the exact failure class this whole area
// has been bitten by, so coverage is asserted as an equality, not a subset.

// seedDistillCorpus writes facts under two clearly separate categories and
// returns the reviewer plus the full set of seed paths.
//
// Category is the lever, not embeddings: with no vectors in the test store the
// neighbour search returns nothing and the subgraph carries no edges, so
// ScopedCluster falls through to its groupByCategory fallback. That makes
// cluster membership deterministic and inspectable — exactly what a test of
// grouping needs.
func seedDistillCorpus(t *testing.T, perCategory int) (*Reviewer, *store.Service, string, map[string]string) {
	t.Helper()

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	branch := "agent/test"
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))
	ctx := context.Background()

	// path -> category, so a test can assert an item never mixes categories.
	categoryOf := map[string]string{}
	body := strings.Repeat("body text that is long enough to matter. ", 40)

	for _, category := range []string{"alpha", "beta"} {
		for i := 0; i < perCategory; i++ {
			f := fact.NewFact(fmt.Sprintf("kb/architecture/%s/seed-%02d.md", category, i))
			f.Title = fmt.Sprintf("%s seed %02d", category, i)
			f.Body = body
			f.Type = fact.Observation
			f.Domain = []string{"architecture", category}
			f.Confidence = 0.7
			f.Sources = 1
			serialized, serr := fact.SerializeFact(f)
			require.NoError(t, serr)
			_, werr := svc.Facts().WriteFact(ctx, branch, f.Path(), serialized, "seed", "")
			require.NoError(t, werr)
			categoryOf[f.Path()] = category
		}
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return NewReviewer(ri, nil), svc, branch, categoryOf
}

// collectDistillItems drains the session queue through the store (answering via
// the pipeline index records an item without applying it, so no RAPTOR
// follow-ups land mid-walk) and returns the fact paths of every depth-0 distill
// item, keyed by cluster key.
func collectDistillItems(t *testing.T, svc *store.Service, sessionID string) map[string][]string {
	t.Helper()
	ctx := context.Background()

	out := map[string][]string{}
	for steps := 0; steps < 500; steps++ {
		item, err := svc.Pipeline().NextPipelineWorkItem(ctx, sessionID)
		require.NoError(t, err)
		if item == nil {
			break
		}
		if item.StepType == "distill" && item.Depth == 0 {
			var facts []factForLLM
			require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &facts),
				"distill item %q payload is not a fact array", item.ClusterKey)
			paths := make([]string, 0, len(facts))
			for _, f := range facts {
				paths = append(paths, f.File)
			}
			out[item.ClusterKey] = paths
		}
		claimed, aerr := svc.Pipeline().AnswerPipelineWorkItem(ctx, item.ID, "{}")
		require.NoError(t, aerr)
		require.True(t, claimed, "draining must claim each item exactly once")
	}
	return out
}

// TestReviewer_DistillCoversEverySeedExactlyOnce is the regression guard on
// grouping. Whatever partition Plan chooses, the union of the depth-0 distill
// items must be exactly the seed pool: no seed dropped, no seed duplicated.
//
// The drop half is the one that bites. filterSmallClusters removes communities
// below minCommunitySize, so grouping straight off ScopedCluster's output would
// silently exclude any seed that clusters alone — invisible in the summary,
// invisible in the response, and indistinguishable from "there was nothing to
// synthesize". That is the same shape as the envelope defect.
func TestReviewer_DistillCoversEverySeedExactlyOnce(t *testing.T) {
	r, svc, _, categoryOf := seedDistillCorpus(t, 6)
	ctx := context.Background()

	res, err := r.StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items := collectDistillItems(t, svc, res.SessionID)
	require.NotEmpty(t, items, "precondition: the session must enqueue depth-0 distill work")

	seen := map[string]int{}
	for _, paths := range items {
		for _, p := range paths {
			seen[p]++
		}
	}

	for p := range categoryOf {
		require.Equalf(t, 1, seen[p],
			"seed %s appears in %d depth-0 distill items; every seed must appear in exactly one", p, seen[p])
	}
	require.Len(t, seen, len(categoryOf),
		"depth-0 distill items must contain exactly the seed pool and nothing else")
}

// paths is a readability helper for the distillGroups tests.
func groupPaths(g distillGroup) []string {
	out := make([]string, 0, len(g.Facts))
	for _, f := range g.Facts {
		out = append(out, f.File)
	}
	return out
}

func seedsNamed(names ...string) []factForLLM {
	out := make([]factForLLM, 0, len(names))
	for _, n := range names {
		out = append(out, factForLLM{File: n, Title: n, Body: "b", Type: "observation"})
	}
	return out
}

// TestDistillGroups_PartitionsByClusterAndExcludesNeighbours is the core of the
// change, tested directly rather than through a session.
//
// Through a session it cannot be observed at all in a unit environment: with no
// embeddings there are no SIMILAR_TO edges, Louvain returns every fact as its
// own community, and filterSmallClusters removes all of them — so clusters
// arrives empty and everything legitimately lands in the remainder. That is
// correct degradation, not a bug, but it means cluster-boundary behaviour has
// to be exercised at the function.
//
// Two properties in one test because they are the same decision: a group is the
// cluster's SEEDS, and only its seeds. Neighbours are in the cluster (search
// pulls them into the subgraph) and must not leak into distill's input.
func TestDistillGroups_PartitionsByClusterAndExcludesNeighbours(t *testing.T) {
	seeds := seedsNamed("kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md", "kb/e.md")

	clusters := [][]factForLLM{
		// A neighbour the search pulled in — in the cluster, never a seed.
		append(seedsNamed("kb/a.md", "kb/b.md"), seedsNamed("kb/neighbour.md")...),
		seedsNamed("kb/c.md", "kb/d.md"),
	}

	groups := distillGroups(seeds, clusters)

	require.Len(t, groups, 3, "two clusters plus a remainder")
	require.Equal(t, "distill-c0", groups[0].Key)
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, groupPaths(groups[0]),
		"a group is the cluster's seeds — the neighbour must not leak into distill's input")
	require.Equal(t, "distill-c1", groups[1].Key)
	require.Equal(t, []string{"kb/c.md", "kb/d.md"}, groupPaths(groups[1]))
	require.Equal(t, "distill-rest", groups[2].Key)
	require.Equal(t, []string{"kb/e.md"}, groupPaths(groups[2]),
		"a seed in no surviving cluster belongs in the remainder, not nowhere")
}

// TestDistillGroups_LoneSeedInClusterFallsToRemainder covers the seed whose
// community held exactly one of them. There is no pattern to find across one
// fact, so it is not its own group — but dropping it would remove it from
// synthesis silently, so it must reappear in the remainder.
func TestDistillGroups_LoneSeedInClusterFallsToRemainder(t *testing.T) {
	seeds := seedsNamed("kb/a.md", "kb/b.md", "kb/c.md")
	clusters := [][]factForLLM{
		seedsNamed("kb/a.md", "kb/b.md"),
		// c clusters only with non-seed neighbours.
		append(seedsNamed("kb/c.md"), seedsNamed("kb/n1.md", "kb/n2.md")...),
	}

	groups := distillGroups(seeds, clusters)

	require.Len(t, groups, 2)
	require.Equal(t, []string{"kb/a.md", "kb/b.md"}, groupPaths(groups[0]))
	require.Equal(t, "distill-rest", groups[1].Key)
	require.Equal(t, []string{"kb/c.md"}, groupPaths(groups[1]))
}

// TestDistillGroups_NoClustersDegradesToOneOrderedGroup pins the degradation
// path, which is what a unit environment and any repo without a populated
// similarity graph actually take. Everything becomes one remainder group — the
// pre-change behaviour — and seed order is preserved, so the same corpus
// produces the same work items run to run rather than wandering with Go's map
// iteration order.
func TestDistillGroups_NoClustersDegradesToOneOrderedGroup(t *testing.T) {
	seeds := seedsNamed("kb/a.md", "kb/b.md", "kb/c.md")

	groups := distillGroups(seeds, nil)

	require.Len(t, groups, 1)
	require.Equal(t, "distill-rest", groups[0].Key)
	require.Equal(t, []string{"kb/a.md", "kb/b.md", "kb/c.md"}, groupPaths(groups[0]),
		"degradation must preserve seed order, not map order")
}
