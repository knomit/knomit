package synthesize

import (
	"context"
	"testing"

	"knomit/internal/fact"
	"knomit/internal/store"

	"github.com/stretchr/testify/require"
)

// selfRefRepoID is a real-shaped 12-hex repo id. It has to be non-empty: with
// an empty localRepoID, fact.ClassifyRef reads the canonical kb://<own-id>/
// form as a FOREIGN ref, so a self-ref filter keyed on RefLocalFact would pass
// these tests without ever seeing the form #280 was produced by.
const selfRefRepoID = "0123456789ab"

// citesSelf reports whether refs name path as a LOCAL fact, in any stored form.
func citesSelf(refs []string, path, localRepoID string) bool {
	self := fact.ClassifyRef(path, localRepoID).Path
	for _, r := range refs {
		if c := fact.ClassifyRef(r, localRepoID); c.Kind == fact.RefLocalFact && c.Path == self {
			return true
		}
	}
	return false
}

// TestDedupCluster_SurvivorDoesNotCiteItself regresses knomit#280. A review
// dedup merged an incident into a gotcha the incident CITED, canonically
// (kb://<id>/kb/…). dedupMergeRefs unioned the loser's refs onto the winner,
// so the survivor cited itself (revision 106d32b3). The ref gate did not stop
// it: its self-ref check exempts carried refs, and the loser carried this one.
//
// Both stored forms of the loser's citation are covered, because the winner's
// path is bare while stored refs are either bare or canonical.
func TestDedupCluster_SurvivorDoesNotCiteItself(t *testing.T) {
	const (
		winnerPath = "kb/gotchas/winner.md"
		loserPath  = "kb/incidents/loser.md"
		liveRef    = "kb/technology/still-here.md"
	)
	for _, tc := range []struct {
		name       string
		loserCites string
	}{
		{"canonical kb:// form", "kb://" + selfRefRepoID + "/" + winnerPath},
		{"bare kb/ form", winnerPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, branch := newSourcesTestRepo(t)

			// The winner is picked on confidence; they must differ, or "the
			// winner survived" is a coin toss.
			const winnerConf, loserConf = 0.9, 0.5
			seedOriginFact(t, svc, branch, liveRef, fact.Observation, fact.Authored, 0.7, 1, nil)
			seedOriginFact(t, svc, branch, winnerPath, fact.Observation, fact.Authored, winnerConf, 1, nil)
			seedOriginFact(t, svc, branch, loserPath, fact.Observation, fact.Authored, loserConf, 1,
				[]string{tc.loserCites, liveRef})

			cluster := []factForLLM{
				{File: winnerPath, Title: "winner", Body: "b", Type: string(fact.Observation), Confidence: winnerConf, Sources: 1},
				{File: loserPath, Title: "loser", Body: "b", Type: string(fact.Observation), Confidence: loserConf, Sources: 1},
			}
			idx := &fixedPairSearch{
				SearchQuery: svc.Search(),
				results:     []store.SearchResult{searchHit(winnerPath), searchHit(loserPath)},
			}

			surviving, err := dedupCluster(ctx, cluster, svc.Facts(), idx, 0.92, "test",
				func(ProgressEvent) {}, branch, selfRefRepoID)
			require.NoError(t, err)
			require.Len(t, surviving, 1)
			require.Equal(t, winnerPath, surviving[0].File)

			rf, err := svc.Facts().ReadFact(ctx, branch, winnerPath, nil)
			require.NoError(t, err)
			merged, err := fact.ParseFact(winnerPath, rf.Content)
			require.NoError(t, err)

			// Precondition on the fixture: the loser's refs really were grafted,
			// or "no self-ref" is true of an empty list.
			require.True(t, citesSelf(merged.Refs, liveRef, selfRefRepoID),
				"the loser's other refs must still be carried onto the winner: %v", merged.Refs)
			require.False(t, citesSelf(merged.Refs, winnerPath, selfRefRepoID),
				"the dedup survivor cites itself: %v", merged.Refs)
			require.True(t, citesSelf(merged.Refs, loserPath, selfRefRepoID),
				"the survivor must cite the fact it subsumed — the record of the merge: %v", merged.Refs)
		})
	}
}

// TestDedupMergeRefs_DropsWinnerSelfRef is the unit half of #280: every form a
// ref naming the winner can take is dropped from the WRITE list, and nothing
// else is.
func TestDedupMergeRefs_DropsWinnerSelfRef(t *testing.T) {
	const (
		winnerPath = "kb/gotchas/winner.md"
		loserPath  = "kb/incidents/loser.md"
		other      = "kb/technology/other.md"
	)
	canonical := "kb://" + selfRefRepoID + "/" + winnerPath

	for _, tc := range []struct {
		name        string
		winnerPath  string
		winnerRefs  []string
		loserRefs   []string
		wantDropped string
	}{
		{"loser cites winner canonically", winnerPath, nil, []string{canonical, other}, canonical},
		{"loser cites winner bare", winnerPath, nil, []string{winnerPath, other}, winnerPath},
		// ClassifyRef lowercases local fact paths, while the winner's path
		// carries the ontology root's real case on disk.
		{"mixed case winner path and ref", "KB/Gotchas/Winner.md", nil,
			[]string{"kb://" + selfRefRepoID + "/kb/gotchas/WINNER.md", other},
			"kb://" + selfRefRepoID + "/kb/gotchas/WINNER.md"},
		// A self-ref the WINNER already carried (written before #132) is
		// dropped by the same filter. Intended: this path rewrites the winner
		// anyway, so it repairs the legacy row rather than re-writing it.
		{"legacy self-ref carried by the winner is dropped too", winnerPath,
			[]string{canonical, other}, nil, canonical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write, carried := dedupMergeRefs(tc.winnerRefs, tc.loserRefs, tc.winnerPath, loserPath, selfRefRepoID)

			require.NotContains(t, write, tc.wantDropped)
			require.False(t, citesSelf(write, tc.winnerPath, selfRefRepoID), "write list cites the winner: %v", write)
			require.Contains(t, write, other, "an unrelated ref must survive the filter")
			require.Contains(t, write, loserPath, "the loser's own path is the record of the merge")

			// The carried set is the operands' snapshot and is NOT filtered
			// (0ee925f4): the gate only judges what is written.
			require.Contains(t, carried, tc.wantDropped)
		})
	}

	t.Run("foreign fact at the same relative path is kept", func(t *testing.T) {
		foreign := "kb://ba9876543210/" + winnerPath
		write, _ := dedupMergeRefs(nil, []string{foreign}, winnerPath, loserPath, selfRefRepoID)
		require.Contains(t, write, foreign, "another repo's fact is not the winner")
	})
}
