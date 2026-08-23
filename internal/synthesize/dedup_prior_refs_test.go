package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"knomit/internal/fact"
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

// seedFactWithRefs writes a fact straight to the store, bypassing every write
// path's ref gate. That is not a test shortcut — it is how a corpus acquires
// facts whose refs no longer resolve: they were written before the gate
// existed, or their target was retracted past the walk-back horizon, or the
// citing repo's history was rewritten. The gate's `prior` parameter exists
// precisely because this state is real and must stay editable.
func seedFactWithRefs(t *testing.T, svc *store.Service, branch, path string, confidence float64, refs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = fact.Observation
	f.Domain = []string{"test"}
	f.Confidence = confidence
	f.Sources = 1
	f.Refs = refs
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestDedupCluster_KeepsUnresolvableCarriedRefs regresses knomit#103: a dedup
// merge grafts the loser's refs onto the winner and used to hand the whole
// union to the ref gate with prior=nil, re-litigating citations both facts
// had carried for months against today's live index. One stale citation
// anywhere in one cluster then aborted the entire review session — every pass
// in the run lost, not just the affected merge — and it aborted again on every
// retry, so the corpus could never be reviewed until the facts were hand-edited.
//
// internal/refs' contract: "A ref is checked ONCE, against the commit the write
// lands on. Refs a fact ALREADY carried are never re-checked." A mechanical
// merge introduces no new citation, so nothing in the union is new.
func TestDedupCluster_KeepsUnresolvableCarriedRefs(t *testing.T) {
	ctx := context.Background()
	branch := "agent/test"

	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	const (
		winnerPath  = "kb/technology/winner.md"
		loserPath   = "kb/technology/loser.md"
		winnerStale = "kb/technology/winner-cited-gone.md"
		loserStale  = "kb/technology/loser-cited-gone.md"
		liveRefPath = "kb/technology/still-here.md"
	)

	// A real, resolvable citation alongside the stale ones, so the assertion
	// below distinguishes "refs survived" from "ref list was emptied".
	seedFactWithRefs(t, svc, branch, liveRefPath, 0.7, nil)
	seedFactWithRefs(t, svc, branch, winnerPath, 0.9, []string{winnerStale, liveRefPath})
	seedFactWithRefs(t, svc, branch, loserPath, 0.5, []string{loserStale})

	cluster := []factForLLM{
		{File: winnerPath, Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: 0.9, Sources: 1},
		{File: loserPath, Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: 0.5, Sources: 1},
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
