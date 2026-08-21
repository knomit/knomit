package synthesize

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

// maxConcurrentDedupSearches caps the number of in-flight idx.Search calls
// during the candidate-pair discovery phase. Same rationale as
// maxConcurrentNeighborSearches in cluster.go.
const maxConcurrentDedupSearches = 8

// mergePair represents two facts that are candidates for merging.
type mergePair struct {
	a          factForLLM
	b          factForLLM
	similarity float64
}

// mergeFacts applies the dedup merge rule to a and b, returning (winner, loser).
// Non-hypothesis facts always win over hypothesis facts regardless of confidence.
// Between same-category facts, winner is higher confidence; ties broken by higher sources.
func mergeFacts(a, b factForLLM) (winner, loser factForLLM) {
	aIsHyp := a.Type == string(fact.Hypothesis)
	bIsHyp := b.Type == string(fact.Hypothesis)

	if aIsHyp != bIsHyp {
		// One is hypothesis, one is not — non-hypothesis always wins.
		if bIsHyp {
			winner, loser = a, b
		} else {
			winner, loser = b, a
		}
	} else {
		// Same category — use confidence/sources tie-breaking.
		if a.Confidence > b.Confidence || (a.Confidence == b.Confidence && a.Sources >= b.Sources) {
			winner, loser = a, b
		} else {
			winner, loser = b, a
		}
	}
	// Merge domains and entities as union.
	winner.Domain = fact.UnionStrings(winner.Domain, loser.Domain)
	winner.Entities = fact.UnionStrings(winner.Entities, loser.Entities)
	// Sources = sum — this is a TRANSFER (the loser is deleted by the caller),
	// so its corroborations move to the winner or are lost.
	//
	// Except a hypothesis loser's, which never counted as corroboration in the
	// first place: computeTransfer skips hypothesis-typed sources and
	// learn.go's subsumeHypothesis adds a ref without adding the count. Pooling
	// it here would launder a conjecture into evidence — write five
	// hypotheses, let dedup absorb them, and the survivor reads sources: 6.
	winner.Sources = winner.Sources + loserCorroborations(loser)
	return winner, loser
}

// loserCorroborations is the sources count a dedup loser contributes to its
// winner: its own, unless it is a hypothesis, which corroborates nothing.
func loserCorroborations(loser factForLLM) int {
	if loser.Type == string(fact.Hypothesis) {
		return 0
	}
	return loser.Sources
}

// applyGreedyMerges sorts pairs by similarity descending and selects pairs
// such that each fact index participates in at most one merge.
func applyGreedyMerges(pairs []mergePair) []mergePair {
	// Sort by similarity descending.
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].similarity > pairs[j].similarity
	})

	consumed := make(map[string]bool)
	var selected []mergePair
	for _, p := range pairs {
		if consumed[p.a.File] || consumed[p.b.File] {
			continue
		}
		selected = append(selected, p)
		consumed[p.a.File] = true
		consumed[p.b.File] = true
	}
	return selected
}

// dedupCluster runs the embedding-based dedup pass on a cluster of facts.
// It searches for near-duplicates, applies greedy merge selection, and
// commits the changes to git and the search index.
// It returns the surviving cluster facts (after removing losers).
//
// Vector lookup uses SearchOptions.QueryByPath, which resolves each member's
// stored vector via a SQL subquery in the sqlite-vec MATCH operand. No
// ONNX inference runs in this pass — every cluster member is already an
// indexed fact whose 768-dim vector lives in facts_vec from when it was
// learned, computed over the same title+body content we'd otherwise
// re-embed here.
func dedupCluster(
	ctx context.Context,
	cluster []factForLLM,
	gs store.FactIndex,
	idx SearchQuery,
	threshold float64,
	recipeName string,
	onProgress func(ProgressEvent),
	agentBranch string,
	// localRepoID is this repo's 12-hex id, for the ref gate below.
	localRepoID string,
) ([]factForLLM, error) {
	if len(cluster) < 2 {
		return cluster, nil
	}

	// The one gate the merged winners below go through, built once for the
	// whole cluster rather than per merge.
	gate := refs.New(localRepoID, refs.FromFactQuery(idx, agentBranch))

	// Build a set for fast path lookup.
	clusterByPath := make(map[string]factForLLM, len(cluster))
	for _, f := range cluster {
		clusterByPath[f.File] = f
	}

	// Find candidate pairs via embedding search. Searches run with bounded
	// concurrency since each idx.Search is independent and dominates wall
	// time. A single mutex protects the seen-pair set and pairs slice.
	seen := make(map[string]bool) // track "a|b" canonical pairs already added
	var pairs []mergePair
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentDedupSearches)
	for _, fact := range cluster {
		g.Go(func() error {
			sq := store.SearchOptions{
				QueryByPath:   fact.File,
				MinSimilarity: threshold,
				Limit:         10,
			}
			results, err := idx.Search(gctx, agentBranch, sq)
			if err != nil {
				return fmt.Errorf("dedupCluster: search for %q: %w", fact.File, err)
			}

			mu.Lock()
			defer mu.Unlock()
			for _, r := range results {
				// Only consider results that are within the current cluster.
				other, inCluster := clusterByPath[r.Path]
				if !inCluster {
					continue
				}
				// Skip self-match.
				if r.Path == fact.File {
					continue
				}
				// Deduplicate symmetric pairs by normalising to (lexicographically smaller, larger).
				key := fact.File + "|" + other.File
				reverseKey := other.File + "|" + fact.File
				if seen[key] || seen[reverseKey] {
					continue
				}
				seen[key] = true

				similarity := r.Score / 100.0
				pairs = append(pairs, mergePair{
					a:          fact,
					b:          other,
					similarity: similarity,
				})
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	if len(pairs) == 0 {
		return cluster, nil
	}

	selected := applyGreedyMerges(pairs)

	// Track which paths are removed (losers).
	removedPaths := make(map[string]bool)

	for _, p := range selected {
		winnerFact, loserFact := mergeFacts(p.a, p.b)

		log.Info().
			Str("winner", winnerFact.File).
			Str("loser", loserFact.File).
			Float64("similarity", p.similarity).
			Msg("dedup: merging near-duplicate facts")

		onProgress(ProgressEvent{Phase: "dedup-merge", Message: fmt.Sprintf("%s <- %s (%.2f)", winnerFact.File, loserFact.File, p.similarity)})

		// Read the winner's full fact from git to get its Refs.
		winnerResult, err := gs.ReadFact(ctx, agentBranch, winnerFact.File, nil)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: read winner %q: %w", winnerFact.File, err)
		}
		fullWinner, err := fact.ParseFact(winnerFact.File, winnerResult.Content)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: parse winner %q: %w", winnerFact.File, err)
		}

		// Read the loser's full fact to get its Refs.
		loserResult, err := gs.ReadFact(ctx, agentBranch, loserFact.File, nil)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: read loser %q: %w", loserFact.File, err)
		}
		fullLoser, err := fact.ParseFact(loserFact.File, loserResult.Content)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: parse loser %q: %w", loserFact.File, err)
		}

		// Apply merged fields to the full winner fact.
		fullWinner.Domain = winnerFact.Domain
		fullWinner.Entities = winnerFact.Entities
		fullWinner.Confidence = winnerFact.Confidence
		fullWinner.Sources = winnerFact.Sources
		// Motifs are merged HERE rather than in mergeFacts above, because
		// factForLLM does not carry them — only the full parsed facts do. Same
		// helper as the learn-time merge: this is the same operation in a second
		// location, and without it the loser's motifs are simply deleted along
		// with the loser, losing authored data that no derived state can rebuild.
		fullWinner.Motifs = fact.MergeMotifs(fullWinner.Motifs, fullLoser.Motifs)
		// Refs = union of both refs + loser's path.
		mergedRefs := fact.UnionStrings(fullWinner.Refs, fullLoser.Refs)
		mergedRefs = fact.AppendUnique(mergedRefs, loserFact.File)

		// Same gate as every other write path. The loser is passed as retracted
		// because it is deleted immediately below and the winner cites it as
		// lineage — that citation is the record of the merge, not a dead ref.
		canonRefs, _, gerr := gate.Apply(ctx, winnerFact.File, mergedRefs, []string{loserFact.File})
		if gerr != nil {
			return nil, fmt.Errorf("dedupCluster: refs for winner %q: %w", winnerFact.File, gerr)
		}
		fullWinner.Refs = canonRefs

		// Serialize and write the winner back to git.
		newContent, err := fact.SerializeFact(fullWinner)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: serialize winner %q: %w", winnerFact.File, err)
		}
		if _, err := gs.WriteFact(ctx, agentBranch, winnerFact.File, newContent, fmt.Sprintf("dedup: merge %s into %s [%s]", loserFact.File, winnerFact.File, recipeName), "subsume"); err != nil {
			return nil, fmt.Errorf("dedupCluster: write winner %q: %w", winnerFact.File, err)
		}

		// Delete the loser from git.
		if _, err := gs.DeleteFact(ctx, agentBranch, loserFact.File, fmt.Sprintf("dedup: remove duplicate %s (merged into %s) [%s]", loserFact.File, winnerFact.File, recipeName)); err != nil {
			return nil, fmt.Errorf("dedupCluster: delete loser %q: %w", loserFact.File, err)
		}

		removedPaths[loserFact.File] = true

		// Update the in-memory fact for the winner in case subsequent searches see it.
		updatedWinner := winnerFact
		updatedWinner.Confidence = fullWinner.Confidence
		updatedWinner.Sources = fullWinner.Sources
		clusterByPath[winnerFact.File] = updatedWinner
	}

	// Build surviving cluster (exclude losers).
	surviving := make([]factForLLM, 0, len(cluster))
	for _, f := range cluster {
		if !removedPaths[f.File] {
			surviving = append(surviving, clusterByPath[f.File])
		}
	}

	return surviving, nil
}
