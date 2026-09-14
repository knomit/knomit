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
		// A hypothesis belongs to subsumeHypothesis, which settles it in the
		// same commit as the observation. Refusing here would block the very
		// write that resolves it, and the refusal advice would be unactionable:
		// knomit_update cannot change a fact's type.
		if r.Type == string(fact.Hypothesis) {
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
// df is looked up by the EXACT string the count was requested for, because the
// two folds do not agree: TokenDF counts over a COLLATE NOCASE column, which
// folds ASCII only, while store.EntityTagMatches folds full Unicode. "Café" and
// "CAFÉ" are therefore one entity to the match and two to the count — each
// reading df 1, clearing the ceiling, and anchoring a refusal that a single
// df-2 entity would not have. Keeping the lookup exact confines the
// disagreement to that non-ASCII case instead of spreading it; reconciling the
// two folds is a follow-up, not something to paper over with a comment claiming
// they already agree.
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

// dfLookup reads the df recorded for exactly this entity string. entityDFFor
// keys the map by the same strings it queried, so an absent key means the
// entity was not asked about rather than that it is rare — treat it as 0 only
// because nonGenericEntities is the sole caller and passes what it queried.
func dfLookup(df map[string]int, entity string) int { return df[entity] }

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
	if len(e.refused) == 1 {
		b.WriteString(" in this call shares")
	} else {
		b.WriteString(" in this call share")
	}
	b.WriteString(" a subject with a fact already on this branch. Nothing was written.\n")
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

// checkSameSubjectCollisions is the read-before-write knomit_learn never had.
//
// It runs after applyDedupMerge so a fact the auto-merge folded is not also
// refused, and before any write so a refusal costs the caller one round trip
// and the corpus nothing. Returns a *sameSubjectRefusal naming every offending
// fact, or nil to let the call proceed.
//
// FIELDS THIS STAGE READS, and what the merge does to each (the CONSEQUENCE
// clause of kb/gotchas/mcp/learn/dedup-merge-stage-ordering): Title and Body
// feed the query text and are INHERITED FROM THE WINNER by mergeFacts; Entities
// are UNIONED; Refs are unioned plus lineage; Origin is inherited from the
// winner and is empty on the struct unless the caller passed it. Every one of
// those is merge-sensitive, and what keeps this stage correct is that it skips
// every index the merge touched — so it only ever reads fields exactly as the
// caller sent them.
//
// Nothing here is durable. Learn is a one-shot call with no session and no
// work-item row, and a refused call must write NOTHING — a counter in a table
// or a private-state fact would turn an all-or-nothing refusal into a partial
// commit. Counting is a structured log event instead, carrying the band and
// model actually in force so a later re-tune is evidence-based.
//
// Every failure mode fails OPEN. A refusal is a hard block on the caller, so an
// infrastructure error, an absent embedder, or a model with no calibrated band
// must let the write through — the same direction applyDedupMerge takes when
// its own search or read fails.
func checkSameSubjectCollisions(
	ctx context.Context,
	s mcpStore,
	branch string,
	inputs []learnFactInput,
	facts []fact.Fact,
	topicCategories []string,
	touched map[int]bool,
	vecs [][]float32,
	emb store.BatchEmbedder,
) error {
	// distinct_from is validated even when nothing would have been refused: a
	// caller naming a path that does not exist has checked nothing, and
	// silently accepting it would let a typo become a permanent bypass. This
	// runs BEFORE the gate-off exits so caller input is validated regardless of
	// embedder state.
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

	// TWO STRUCTURAL EXITS, both logged so a silently-disabled gate is visible.
	//
	// (1) No embedder. Same nil predicate store.EmbedderThresholds treats as
	// "embeddings disabled". There is nothing to compare and no model whose
	// geometry would apply.
	if emb == nil {
		log.Info().Msg("learn: same-subject gate off, embeddings disabled")
		return nil
	}
	modelID := emb.ID()
	// (2) No calibrated band for the model actually running. The band is read
	// from params.ForModel keyed by that model's id, NOT from
	// EmbedderThresholds — which falls back to the NOMIC set for anything it
	// cannot answer, and would therefore judge one model's facts against
	// another's cosine distribution with nothing to show for it. ForModel's
	// bool is what makes "we have no geometry for this" reachable at all.
	//
	// This DOES leave two sources for Dedup: applyDedupMerge reads it from
	// EmbedderThresholds, this gate from ForModel. They cannot diverge in
	// production — descriptors read their thresholds from params, and
	// TestEveryDescriptorReadsItsThresholdsFromParams enforces it — and where
	// ForModel has no entry this gate is OFF, so no band comparison happens and
	// there is nothing to disagree about. Only a test embedder declaring
	// thresholds its model id does not carry can see the split. Do NOT
	// "harmonise" this by moving applyDedupMerge onto ForModel: dedup must keep
	// working for embedders params does not know, so it would need a fallback —
	// reintroducing the silent nomic default this gate exists to avoid.
	th, ok := params.ForModel(modelID)
	if !ok {
		log.Warn().Str("model", modelID).
			Msg("learn: same-subject gate off, no calibrated thresholds for this embedding model")
		return nil
	}
	if len(vecs) == 0 {
		// An embedder exists but the batch embed failed; dedupEmbed already
		// warned. Without vectors there is nothing to compare.
		return nil
	}

	total, err := s.factQuery.LiveFactCount(ctx, branch)
	if err != nil {
		log.Warn().Err(err).Msg("learn: same-subject gate skipped, corpus size unavailable")
		return nil
	}
	ceiling := dfCeilingForFacts(total)

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
		// Pipeline output is exempt. The synthesis and discovery prompts
		// construct a fact to carry the union of its sources' entities and to
		// cite them, so it collides with its own sources BY CONSTRUCTION; and
		// that output already passes through the review prune, which is the
		// gate designed for it. Origin is only set when the caller passed it
		// (learn.go leaves it empty otherwise and defaults at serialize time),
		// so this limb is deliberately not the one doing the work — the refs
		// limb below is, and it is origin-independent.
		if f.Origin == fact.Distilled || f.Origin == fact.Discovered {
			log.Info().Int("fact", i).Str("origin", string(f.Origin)).Str("model", modelID).
				Msg("learn: same-subject gate skipped for pipeline-origin fact")
			continue
		}
		entityDF, err := entityDFFor(ctx, s, branch, f.Entities)
		if err != nil {
			log.Warn().Err(err).Int("fact", i).Msg("learn: same-subject gate skipped, entity df unavailable")
			continue
		}
		candidates, err := findSameSubjectCandidates(
			ctx, s.factQuery, branch, f, vecs[i], th, entityDF, ceiling, defaultPageSize)
		if err != nil {
			log.Warn().Err(err).Int("fact", i).Msg("learn: same-subject gate skipped, search failed")
			continue
		}
		kept := candidates[:0:0]
		for _, c := range candidates {
			// Two bypasses, logged separately so each one's rate is readable on
			// its own. The refs limb is the cheaper escape — any caller can add
			// the colliding path to refs — so it needs at least as much
			// visibility as distinct_from, or the telemetry under-reports.
			switch {
			case namedIn(inputs[i].DistinctFrom, c.Path):
				logBypass(i, c, th, modelID, "learn: same-subject candidate bypassed by distinct_from")
			case citedIn(f.Refs, c.Path):
				logBypass(i, c, th, modelID, "learn: same-subject candidate skipped, already cited in refs")
			default:
				kept = append(kept, c)
			}
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

func logBypass(i int, c sameSubjectCandidate, th params.Thresholds, modelID, msg string) {
	log.Info().
		Int("fact", i).
		Str("candidate", c.Path).
		Strs("shared_entities", c.SharedEntities).
		Float64("similarity", c.Similarity).
		Float64("band_low", th.ReflectNovelty).
		Float64("band_high", th.Dedup).
		Str("model", modelID).
		Msg(msg)
}

// citedIn reports whether the incoming fact already cites this candidate. A
// cited candidate is declared lineage — the caller has said how the two relate,
// which is the thing a refusal exists to make them do. Refs may carry a bare
// path or the canonical kb://<repo-id>/<path> form (canonicalisation happens
// later in the handler), so the suffix is what is compared. Case-folded for the
// same reason namedIn is: fact paths are lowercase-canonical.
func citedIn(refs []string, target string) bool {
	t := strings.ToLower(target)
	for _, r := range refs {
		r = strings.ToLower(r)
		if r == t || strings.HasSuffix(r, "/"+t) {
			return true
		}
	}
	return false
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
		if strings.ToLower(p) == strings.ToLower(target) {
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
