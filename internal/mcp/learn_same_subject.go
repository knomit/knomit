package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

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

// sameSubjectRefusal is the whole call's refusal, carrying every offending
// fact so one round trip tells the caller about all of them.
//
// The call is refused WHOLE, never in part: facts in one learn call commit in
// one commit, so writing the innocent ones would report success for a batch
// that only half landed.
type sameSubjectRefusal struct {
	refused []refusedFact
}

type refusedFact struct {
	Index      int
	Title      string
	Candidates []sameSubjectCandidate
}

func (e *sameSubjectRefusal) Error() string {
	var b strings.Builder
	b.WriteString("refused: ")
	b.WriteString(pluralFacts(len(e.refused)))
	b.WriteString(" in this call share a subject with a fact already on this branch. Nothing was written.\n")
	for _, rf := range e.refused {
		fmt.Fprintf(&b, "\nfact %d: %q\n", rf.Index, rf.Title)
		for _, c := range rf.Candidates {
			b.WriteString("  ")
			b.WriteString(c.String())
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTo update the existing fact call knomit_update on its path. " +
		"To assert this is a different fact, resubmit with distinct_from: [<paths>] on this entry.")
	return b.String()
}

// embedderID names the model in telemetry, so a refusal rate can later be read
// against the geometry that produced it.
func embedderID(emb store.BatchEmbedder) string {
	if emb == nil {
		return "none"
	}
	return emb.ID()
}

// checkSameSubjectCollisions is the read-before-write knomit_learn never had.
//
// It runs after applyDedupMerge so a fact the auto-merge folded is not also
// refused, and before any write so a refusal costs the caller one round trip
// and the corpus nothing. Returns a *sameSubjectRefusal naming every offending
// fact, or nil to let the call proceed.
//
// Nothing here is durable. Learn is a one-shot call with no session and no
// work-item row, and a refused call must write NOTHING — a counter in a table
// or a private-state fact would turn an all-or-nothing refusal into a partial
// commit. Counting is a structured log event instead, carrying the band and
// model actually in force so a later re-tune is evidence-based.
func checkSameSubjectCollisions(
	ctx context.Context,
	s mcpStore,
	branch string,
	inputs []learnFactInput,
	facts []fact.Fact,
	topicCategories []string,
	touched map[int]bool,
	vecs [][]float32,
	th params.Thresholds,
	modelID string,
) error {
	// distinct_from is validated even when nothing would have been refused: a
	// caller naming a path that does not exist has checked nothing, and
	// silently accepting it would let a typo become a permanent bypass.
	for i, in := range inputs {
		for _, p := range in.DistinctFrom {
			exists, err := s.facts.FactExists(ctx, branch, p)
			if err != nil {
				return fmt.Errorf("fact %d: distinct_from %s: %v", i, p, err)
			}
			if !exists {
				return fmt.Errorf(
					"fact %d: distinct_from names %s, which does not exist on this branch; "+
						"name only paths returned by a refusal or by knomit_query", i, p)
			}
		}
	}

	// No vectors means no embedder, or a batch embed that failed. Either way
	// there is nothing to judge against, and EmbedderThresholds would hand back
	// the NOMIC band while a different model — or none — is running. Fail OPEN:
	// a refusal is a hard block on the caller and must never rest on a signal
	// we do not have.
	if len(vecs) == 0 || th.ReflectNovelty <= 0 || th.Dedup <= 0 {
		return nil
	}

	stats, err := s.factQuery.Stats(ctx, branch, "", "")
	if err != nil {
		return fmt.Errorf("same-subject: corpus size: %v", err)
	}
	// One Stats call per learn call, not per fact. Its first query is the
	// COUNT(*) we want; the GROUP BYs it also runs are discarded. Cheap enough
	// here and it adds no store method — worth revisiting if learn ever becomes
	// hot. Its own Entities map is deliberately NOT used for df: it is keyed by
	// raw JSON value and is not case-folded, so it would split "Ramp"/"ramp".
	ceiling := dfCeilingForFacts(stats.Total)

	var refusals []refusedFact
	for i := range facts {
		// Private state is not knowledge, has no subject to collide on, and is
		// unindexed — the search would return nothing anyway.
		if topicCategories[i] == "" || touched[i] {
			continue
		}
		if i >= len(vecs) || len(vecs[i]) == 0 {
			continue
		}
		f := facts[i]
		entityDF, err := entityDFFor(ctx, s, branch, f.Entities)
		if err != nil {
			return fmt.Errorf("fact %d: entity df: %v", i, err)
		}
		candidates, err := findSameSubjectCandidates(
			ctx, s.factQuery, branch, f, vecs[i], th, entityDF, ceiling, defaultPageSize)
		if err != nil {
			return fmt.Errorf("fact %d: same-subject search: %v", i, err)
		}
		kept := candidates[:0:0]
		for _, c := range candidates {
			if namedIn(inputs[i].DistinctFrom, c.Path) {
				// A bypass is logged with the same fields as a refusal, so a
				// reflex use of the escape is as visible as the refusals it
				// suppresses.
				log.Info().
					Int("fact", i).
					Str("candidate", c.Path).
					Strs("shared_entities", c.SharedEntities).
					Float64("similarity", c.Similarity).
					Float64("band_low", th.ReflectNovelty).
					Float64("band_high", th.Dedup).
					Str("model", modelID).
					Msg("learn: same-subject candidate bypassed by distinct_from")
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			continue
		}
		log.Info().
			Int("fact", i).
			Int("candidates", len(kept)).
			Strs("anchors", anchorsOf(kept)).
			Float64("band_low", th.ReflectNovelty).
			Float64("band_high", th.Dedup).
			Str("model", modelID).
			Msg("learn: refused, fact shares a subject with an existing fact")
		refusals = append(refusals, refusedFact{Index: i, Title: f.Title, Candidates: kept})
	}
	if len(refusals) == 0 {
		return nil
	}
	return &sameSubjectRefusal{refused: refusals}
}

// entityDFFor reads each entity's document frequency through TokenDF, the store's
// one definition of df. It counts over a COLLATE NOCASE column, which is the
// same case-fold rule store.EntityTagMatches applies — so the count and the
// shared-entity match agree on what one entity is.
func entityDFFor(ctx context.Context, s mcpStore, branch string, entities []string) (map[string]int, error) {
	out := make(map[string]int, len(entities))
	for _, e := range entities {
		if e == "" {
			continue
		}
		if _, done := out[e]; done {
			continue
		}
		n, err := s.graph.TokenDF(ctx, branch, e, "entity")
		if err != nil {
			return nil, err
		}
		out[e] = n
	}
	return out, nil
}

// namedIn matches case-INSENSITIVELY because the validation above does.
// FactExists lowercases before looking a path up (fact paths are
// lowercase-canonical), so a mixed-case distinct_from entry passes validation.
// An exact == here would then fail to match the candidate, and the caller would
// be refused a second time by a message telling them to do precisely what they
// just did — a dead end escapable only by guessing the casing rule. The two
// paths must agree on what one path IS, for the same reason sharedAnchors and
// the df lookup must agree on what one entity is.
func namedIn(paths []string, target string) bool {
	for _, p := range paths {
		if strings.EqualFold(p, target) {
			return true
		}
	}
	return false
}

func anchorsOf(cs []sameSubjectCandidate) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		for _, e := range c.SharedEntities {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}
