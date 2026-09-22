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

// TestDirtyFacts_ExcludesSignalFacts is the signal-type half of the rule above.
// It is a sibling rather than another case inside that test because it asserts
// something the policy/heuristic case does not: that the filter was actually
// REACHED.
//
// A NotContains on its own passes for the wrong reason just as happily as the
// right one — a fact that failed to write, or failed to parse, is also not in
// the seed set. So each subtest reads both facts back and parses them before
// looking at the seeds: two facts on the branch, both well-formed, exactly one
// of them a seed. Signals are excluded by KIND, inheriting the guarantee whole;
// the value of pinning it is that nothing about `signal` may quietly opt back
// in later.
//
// The two subtests do NOT exercise the same filter, which is worth knowing
// before trusting either alone. SeedQuery pushes IncludeKinds:[epistemic] into
// SQL, so on the full-scan path a pragmatic fact never comes back from the
// index and AcceptSeed is never asked about it — that subtest pins the SQL
// filter. Only the incremental path parses every changed file and puts it to
// AcceptSeed, so that subtest is the one pinning the authoritative rule.
// Verified by mutation: forcing AcceptSeed to `return true` fails the
// incremental subtest and leaves the full-scan one passing. Calling the SQL
// clause "purely an efficiency measure" is true of what it is FOR and false of
// what it currently DOES on that path.
func TestDirtyFacts_ExcludesSignalFacts(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	const epPath = "kb/technology/obs2.md"
	const sigPath = "kb/tasks/inbox/mindev-local-8ef0cd32/sig.md"

	t.Run("first run (index path)", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		writeKindFact(t, svc, branch, sigPath, fact.Pragmatic, fact.Signal)
		requireParsedOnBranch(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		requireParsedOnBranch(t, svc, branch, sigPath, fact.Pragmatic, fact.Signal)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		seeds, err := r.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		paths := seedPaths(seeds)
		require.Contains(t, paths, epPath, "the observation must be selected")
		require.NotContains(t, paths, sigPath, "a signal must never seed synthesis")
		// The fixture seeds an unrelated fact of its own, so the corpus-wide
		// count is not 1 here; of the two facts under test exactly one seeds.
		// What excludes the signal HERE is the SQL kind filter in SeedQuery,
		// not AcceptSeed — see the note on this test.
		require.Equal(t, 1, countOf(paths, epPath, sigPath),
			"exactly one of the two facts written is a seed")
	})

	t.Run("incremental (diff path)", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/baseline2.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))

		writeKindFact(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		writeKindFact(t, svc, branch, sigPath, fact.Pragmatic, fact.Signal)
		requireParsedOnBranch(t, svc, branch, epPath, fact.Epistemic, fact.Observation)
		requireParsedOnBranch(t, svc, branch, sigPath, fact.Pragmatic, fact.Signal)

		gs, idx, pipelineIdx, _ := r.storeIndices()
		seeds, err := r.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
		require.NoError(t, err)

		// Only the two facts written after the watermark are dirty, so the
		// whole seed set is the assertion: two facts changed, one seeded.
		// This is the subtest with teeth on AcceptSeed itself: the diff path
		// parses each changed file and asks the filter, with no SQL kind
		// clause ahead of it.
		require.Len(t, seeds, 1, "two facts changed since the watermark; only the observation seeds")
		require.Equal(t, epPath, seeds[0].File)
	})
}

// requireParsedOnBranch reads a fact back off the branch and parses it, so a
// later "not a seed" assertion cannot be satisfied by a fact that was never
// written or cannot be read. It returns nothing: the point is the failure.
func requireParsedOnBranch(t *testing.T, svc *store.Service, branch, path string, kind fact.Kind, typ fact.Type) {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoErrorf(t, err, "fact %q must exist on the branch", path)
	f, err := fact.ParseFact(path, res.Content)
	require.NoErrorf(t, err, "fact %q must parse", path)
	require.Equal(t, kind, f.Kind)
	require.Equal(t, typ, f.Type)
}

func countOf(haystack []string, wanted ...string) int {
	n := 0
	for _, w := range wanted {
		for _, h := range haystack {
			if h == w {
				n++
				break
			}
		}
	}
	return n
}
