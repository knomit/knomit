package synthesize

import (
	"context"
	"fmt"
	"sort"

	"github.com/rs/zerolog/log"
	"knomit/internal/fact"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

const defaultDedupThreshold = 0.92

// mergePair represents two facts that are candidates for merging.
type mergePair struct {
	a          factForLLM
	b          factForLLM
	similarity float64
}

// mergeFacts applies the dedup merge rule to a and b, returning (winner, loser).
// Winner is the fact with higher confidence; ties are broken by higher sources.
func mergeFacts(a, b factForLLM) (winner, loser factForLLM) {
	if a.Confidence > b.Confidence || (a.Confidence == b.Confidence && a.Sources >= b.Sources) {
		winner, loser = a, b
	} else {
		winner, loser = b, a
	}
	// Merge domains and entities as union.
	winner.Domain = fact.UnionStrings(winner.Domain, loser.Domain)
	winner.Entities = fact.UnionStrings(winner.Entities, loser.Entities)
	// Confidence = max (already winner's).
	// Sources = sum.
	winner.Sources = a.Sources + b.Sources
	return winner, loser
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
func dedupCluster(
	ctx context.Context,
	cluster []factForLLM,
	gs GitStore,
	idx SearchIndex,
	threshold float64,
	recipeName string,
	onProgress func(ProgressEvent),
	embedders ...Embedder,
) ([]factForLLM, error) {
	if len(cluster) < 2 {
		return cluster, nil
	}

	// Build a set for fast path lookup.
	clusterByPath := make(map[string]factForLLM, len(cluster))
	for _, f := range cluster {
		clusterByPath[f.File] = f
	}

	// Batch-embed all cluster facts upfront if a BatchEmbedder is available.
	var clusterVecs [][]float32
	if len(embedders) > 0 {
		if batcher, ok := embedders[0].(BatchEmbedder); ok {
			texts := make([]string, len(cluster))
			for i, f := range cluster {
				texts[i] = f.Title + " " + f.Body
			}
			clusterVecs, _ = batcher.EmbedBatch(texts)
		}
	}

	// Find candidate pairs via embedding search.
	seen := make(map[string]bool) // track "a|b" canonical pairs already added
	var pairs []mergePair

	for i, fact := range cluster {
		sq := store.SearchQuery{
			Text:          fact.Title + " " + fact.Body,
			MinSimilarity: threshold,
			Limit:         10,
		}
		if clusterVecs != nil && i < len(clusterVecs) && len(clusterVecs[i]) > 0 {
			sq.QueryVec = clusterVecs[i]
		}
		results, err := idx.Search(sq)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: search for %q: %w", fact.File, err)
		}

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
		winnerContent, err := gs.ReadFile(winnerFact.File)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: read winner %q: %w", winnerFact.File, err)
		}
		fullWinner, err := mcp.ParseFact(winnerFact.File, winnerContent)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: parse winner %q: %w", winnerFact.File, err)
		}

		// Read the loser's full fact to get its Refs.
		loserContent, err := gs.ReadFile(loserFact.File)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: read loser %q: %w", loserFact.File, err)
		}
		fullLoser, err := mcp.ParseFact(loserFact.File, loserContent)
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: parse loser %q: %w", loserFact.File, err)
		}

		// Apply merged fields to the full winner fact.
		fullWinner.Domain = winnerFact.Domain
		fullWinner.Entities = winnerFact.Entities
		fullWinner.Confidence = winnerFact.Confidence
		fullWinner.Sources = winnerFact.Sources
		// Refs = union of both refs + loser's path.
		mergedRefs := fact.UnionStrings(fullWinner.Refs, fullLoser.Refs)
		mergedRefs = fact.AppendUnique(mergedRefs, loserFact.File)
		fullWinner.Refs = mergedRefs

		// Serialize and write the winner back to git.
		newContent := mcp.SerializeFact(fullWinner)
		commitHash, blobHash, err := gs.WriteFile(winnerFact.File, newContent, fmt.Sprintf("dedup: merge %s into %s [%s]", loserFact.File, winnerFact.File, recipeName), "subsume")
		if err != nil {
			return nil, fmt.Errorf("dedupCluster: write winner %q: %w", winnerFact.File, err)
		}

		// Update the search index for the winner.
		if err := idx.Upsert(store.FactRecord{
			Path:       fullWinner.Path,
			Title:      fullWinner.Title,
			BlobHash:   blobHash,
			Type:       string(fullWinner.Type),
			Domain:     fullWinner.Domain,
			Entities:   fullWinner.Entities,
			Confidence: fullWinner.Confidence,
			Sources:    fullWinner.Sources,
			Refs:       fullWinner.Refs,
			CommitHash: commitHash,
		}); err != nil {
			return nil, fmt.Errorf("dedupCluster: upsert winner %q: %w", winnerFact.File, err)
		}

		// Delete the loser from git and the search index.
		if _, err := gs.DeleteFile(loserFact.File, fmt.Sprintf("dedup: remove duplicate %s (merged into %s) [%s]", loserFact.File, winnerFact.File, recipeName), "retract"); err != nil {
			return nil, fmt.Errorf("dedupCluster: delete loser %q: %w", loserFact.File, err)
		}
		if err := idx.Delete(loserFact.File); err != nil {
			return nil, fmt.Errorf("dedupCluster: index delete loser %q: %w", loserFact.File, err)
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
