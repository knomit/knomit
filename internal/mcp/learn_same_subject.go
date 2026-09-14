package mcp

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// factSearcher is the single FactQuery method this stage needs. Narrowing the
// dependency is what lets the unit tests fake one method instead of the dozen
// store.FactQuery declares; store.FactQuery satisfies it unchanged.
type factSearcher interface {
	Search(ctx context.Context, branch string, q store.SearchOptions) ([]store.SearchResult, error)
}

// sameSubjectCandidate is one existing fact that shares a subject with an
// incoming one: similar enough to be about the same thing, not similar enough
// for the auto-merge floor to have folded them, and anchored on at least one
// shared NON-GENERIC entity so text similarity alone cannot produce it.
type sameSubjectCandidate struct {
	Path           string
	Title          string
	SharedEntities []string
	Similarity     float64
}

// String is the line the refusal message shows the caller, so the rendering
// lives with the type rather than being re-spelled at each call site.
func (c sameSubjectCandidate) String() string {
	return fmt.Sprintf("%s — %s (shared: %s; similarity %.2f)",
		c.Path, c.Title, strings.Join(c.SharedEntities, ", "), c.Similarity)
}

// dfCeilingForFacts is the document-frequency ceiling above which a label has
// "gone generic" on a corpus of n live facts. It delegates to the motif df
// band's rule rather than restating it: the rule is a RATIO of the corpus's
// own size with a small-corpus floor, so it resolves to a different df on
// every corpus and encodes no fixed corpus property.
//
// NOTE the rule was derived on the MOTIF axis. Entities are more numerous and
// longer-tailed — every fact carries entities, not every fact carries motifs —
// so this ceiling is reused for its SHAPE, not because 2% has been validated
// against the entity distribution. See the PR: measuring known collision pairs'
// anchors against it is the check that would confirm the transfer.
func dfCeilingForFacts(n int) int { return synthesize.DFCeiling(n) }

// findSameSubjectCandidates searches the WHOLE branch (not the category
// directory — that scoping is why two sessions filing one event under two
// categories never collide at write time) for facts in (ReflectNovelty, Dedup)
// that share >= 1 NON-GENERIC entity with f.
//
// Every "no anchor" branch fails OPEN, returning nil so nothing is refused:
// f carries no entities, none of the shared entities is specific enough to
// anchor on, there is no calibrated band, or no query vector was donated.
// A refusal is a hard block on the caller, and imposing one on an absent or
// untrustworthy signal is worse than letting the write through — that is the
// failure kb/decisions/lens/no-write-time-coherence-gate names.
//
// The band is open at the bottom because the store's MinSimilarity filter is
// strict (cosine > min), and closed at the top because a hit at or above Dedup
// belongs to applyDedupMerge, which has already folded it.
//
// SearchResult.Score is cosine*100 on the vector path (search_query.go, Score:
// c.score * 100.0) while params.Thresholds are 0-1, so it is divided before
// any comparison. Comparing raw would put every score above Dedup and refuse
// nothing, silently.
func findSameSubjectCandidates(
	ctx context.Context,
	q factSearcher,
	branch string,
	f fact.Fact,
	vec []float32,
	th params.Thresholds,
	entityDF map[string]int,
	dfCeiling int,
	limit int,
) ([]sameSubjectCandidate, error) {
	// No calibrated band means no embedder: EmbedderThresholds(nil) hands back
	// the NOMIC fallback, so judging candidates here would apply one model's
	// cosine cutoffs while a different model — or none — is running. Nothing is
	// refused today only because an empty vector map makes the store return no
	// rows before scoring; this makes the safe answer structural.
	if th.ReflectNovelty <= 0 || th.Dedup <= 0 {
		return nil, nil
	}
	anchors := nonGenericEntities(f.Entities, entityDF, dfCeiling)
	if len(anchors) == 0 {
		return nil, nil
	}

	sq := store.SearchOptions{
		Text:          f.Title + " " + f.Body,
		MinSimilarity: th.ReflectNovelty,
		Limit:         limit,
	}
	if len(vec) > 0 {
		sq.QueryVec = vec
	}
	results, err := q.Search(ctx, branch, sq)
	if err != nil {
		return nil, err
	}

	var out []sameSubjectCandidate
	for _, r := range results {
		cosine := r.Score / 100.0
		if cosine <= th.ReflectNovelty || cosine >= th.Dedup {
			continue
		}
		shared := sharedAnchors(anchors, r.Entities)
		if len(shared) == 0 {
			continue
		}
		out = append(out, sameSubjectCandidate{
			Path:           r.Path,
			Title:          r.Title,
			SharedEntities: shared,
			Similarity:     cosine,
		})
	}
	return out, nil
}

// nonGenericEntities keeps only the entities specific enough to anchor a
// refusal. An entity carried by more facts than the ceiling (MCP, Claude,
// Anthropic on this corpus) says nothing about two facts being the same
// subject, so it cannot produce a refusal on its own.
//
// df lookup is case-folded because TokenDF counts over a COLLATE NOCASE column
// and store.EntityTagMatches folds too — the count and the match must agree on
// what one entity is, or a fact could be refused on an entity whose df was
// read as 0.
func nonGenericEntities(entities []string, entityDF map[string]int, ceiling int) []string {
	var out []string
	for _, e := range entities {
		if e == "" {
			continue
		}
		if dfLookup(entityDF, e) > ceiling {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sharedAnchors intersects the incoming fact's anchor entities with an existing
// fact's, preserving the incoming order so the refusal message reads the way
// the caller wrote it. Comparison is store.EntityTagMatches (case-fold only) —
// NOT the de-hyphenising CanonicalizeTag beside it, which would make "AI-Index"
// and "AI Index" the same entity and widen every refusal.
func sharedAnchors(anchors []string, existing []string) []string {
	var out []string
	for _, a := range anchors {
		for _, e := range existing {
			if store.EntityTagMatches(e, a) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// dfLookup reads a df map case-insensitively, so a caller may key it by the
// entity form it had to hand.
func dfLookup(df map[string]int, entity string) int {
	if n, ok := df[entity]; ok {
		return n
	}
	for k, n := range df {
		if store.EntityTagMatches(k, entity) {
			return n
		}
	}
	return 0
}
