package synthesize

import (
	"context"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// fixedPairSearch is a SearchQuery whose Search returns a canned result set,
// so a dedup pair can be forced without standing up an embedder. Every other
// method — crucially FactExistsAt, which the ref gate resolves through — is
// the real one.
type fixedPairSearch struct {
	SearchQuery
	results []store.SearchResult
}

func (f *fixedPairSearch) Search(context.Context, string, store.SearchOptions) ([]store.SearchResult, error) {
	return f.results, nil
}

// searchHit is a result above any dedup threshold — dedupCluster divides Score
// by 100 to get the cosine it compares against the threshold.
func searchHit(path string) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: path}},
		Score:        96,
	}
}

// TestDedupCluster_KeepsUnresolvableCarriedRefs regresses knomit#103: a dedup
// merge grafts the loser's refs onto the winner and used to hand the whole
// union to the ref gate with only the loser's path as prior, re-litigating
// citations both facts had carried for months against today's live index. One
// stale citation anywhere in one cluster then aborted the entire review
// session — every pass in the run lost, not just the affected merge — and it
// aborted again on every retry, so the corpus could never be reviewed until
// the facts were hand-edited.
//
// internal/refs' contract: "A ref is checked ONCE, against the commit the write
// lands on. Refs a fact ALREADY carried are never re-checked." A mechanical
// merge introduces no new citation, so nothing in the union is new.
//
// The seeds go in through the store rather than a write path, which is not a
// test shortcut: it is how a corpus acquires facts whose refs no longer
// resolve — written before the gate existed, or their target retracted past
// the walk-back horizon, or the citing repo's history rewritten. The gate's
// `prior` parameter exists precisely because this state is real and must stay
// editable.
func TestDedupCluster_KeepsUnresolvableCarriedRefs(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	const (
		winnerPath  = "kb/technology/winner.md"
		loserPath   = "kb/technology/loser.md"
		winnerStale = "kb/technology/winner-cited-gone.md"
		loserStale  = "kb/technology/loser-cited-gone.md"
		liveRefPath = "kb/technology/still-here.md"
	)

	// Precondition: the confidences that pick the winner must actually differ,
	// or "the winner survived" is an assertion about a coin toss.
	const winnerConf, loserConf = 0.9, 0.5
	require.NotEqual(t, winnerConf, loserConf)

	// A real, resolvable citation alongside the stale ones, so the assertion
	// below distinguishes "refs survived" from "ref list was emptied".
	seedOriginFact(t, svc, branch, liveRefPath, fact.Observation, fact.Authored, 0.7, 1, nil)
	seedOriginFact(t, svc, branch, winnerPath, fact.Observation, fact.Authored, winnerConf, 1,
		[]string{winnerStale, liveRefPath})
	seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, loserConf, 1,
		[]string{loserStale})

	cluster := []factForLLM{
		{File: winnerPath, Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: winnerConf, Sources: 1},
		{File: loserPath, Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: loserConf, Sources: 1},
	}
	idx := &fixedPairSearch{
		SearchQuery: svc.Search(),
		results: []store.SearchResult{
			searchHit(winnerPath),
			searchHit(loserPath),
		},
	}

	surviving, err := dedupCluster(ctx, cluster, svc.Facts(), idx, 0.92, "test",
		func(ProgressEvent) {}, branch, bareRefFixture)
	require.NoError(t, err, "a carried ref that no longer resolves must not abort the dedup pass")

	require.Len(t, surviving, 1)
	require.Equal(t, winnerPath, surviving[0].File)

	rf, err := svc.Facts().ReadFact(ctx, branch, winnerPath, nil)
	require.NoError(t, err)
	merged, err := fact.ParseFact(winnerPath, rf.Content)
	require.NoError(t, err)

	// Provenance survives: dropping the unresolvable refs to satisfy a check
	// that should never have run on them destroys the lineage the merge exists
	// to preserve.
	require.Contains(t, merged.Refs, winnerStale, "winner's own carried ref was dropped")
	require.Contains(t, merged.Refs, loserStale, "loser's carried ref was not grafted onto the winner")
	require.Contains(t, merged.Refs, liveRefPath)
	require.Contains(t, merged.Refs, loserPath, "the merge must cite the fact it subsumed")
}

// TestDedupMergeRefs_CarriedIsAnIndependentSnapshot guards 0ee925f4: passing
// the same slice as both `refs` and `prior` to refs.Gate.Apply exempts every
// local ref in the write, unconditionally, and the call still compiles, still
// canonicalizes, and still reads like a gate. Because this merge's write list
// happens to be entirely carried today, the aliased form would be correct FOR
// TODAY'S CODE — and would launder the first ref a later change appends here.
//
// So the property under test is not "prior equals the write list" (it does),
// it is "prior was built from the operands and can diverge from the write
// list". The two assertions below are the two halves of that, and each is
// meant to go red under its own sabotage: return `carried = write` from the
// helper, and the aliasing check fails; build `carried` from `write` after a
// later append, and the divergence check fails.
func TestDedupMergeRefs_CarriedIsAnIndependentSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	const (
		winnerPath = "kb/technology/winner.md"
		loserPath  = "kb/technology/loser.md"
		staleRef   = "kb/technology/cited-gone.md"
		liveRef    = "kb/technology/still-here.md"
	)
	seedOriginFact(t, svc, branch, liveRef, fact.Observation, fact.Authored, 0.7, 1, nil)
	seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, 0.5, 1, nil)

	winnerRefs := []string{staleRef}
	loserRefs := []string{liveRef}
	// Precondition: the operands must carry DIFFERENT refs, or a union that
	// dropped one of them would still look right.
	require.NotEqual(t, winnerRefs, loserRefs)

	write, carried := dedupMergeRefs(winnerRefs, loserRefs, loserPath)

	// The merge's own behaviour is unchanged: union of both, plus the loser.
	require.ElementsMatch(t, []string{staleRef, liveRef, loserPath}, write)

	// Structurally satisfied today — nothing in the write list is new — which
	// is what keeps a carried-but-unresolvable ref out of the gate's way.
	require.ElementsMatch(t, write, carried)

	// ...but not by aliasing. Same backing array means `prior` can never
	// differ from `refs`, whatever a later change does to the write list.
	require.NotSame(t, &write[0], &carried[0],
		"prior must be its own list, not the write list under a second name")

	gate := refs.New(bareRefFixture, refs.FromFactQuery(svc.Search(), branch))

	// Positive control: the carried set, unresolvable member and all, passes.
	// This is the #103 case and it must not be rejected.
	_, _, err := gate.Apply(ctx, winnerPath, write, carried)
	require.NoError(t, err, "a ref both operands carried must not be re-judged")

	// The divergence the snapshot exists for: a ref the merge did not carry,
	// of the kind a later change might append here (a lineage pointer to a
	// synthesized parent), is still gated.
	const ghost = "kb/technology/never-existed.md"
	extended := append(append([]string(nil), write...), ghost)
	_, _, err = gate.Apply(ctx, winnerPath, extended, carried)
	require.Error(t, err, "a ref that was never carried must still be rejected")
	require.Contains(t, err.Error(), ghost)
}
