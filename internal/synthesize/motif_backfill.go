package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/textnorm"
)

// Backfill: offering an existing motif-less fact a motif (§6, §11).
//
// This is the ONE prompt in the system that carries corpus vocabulary, and the
// exception is deliberate and narrow (MN1, designer ruling Q8). MN1's concern
// is write-time bias: an agent AUTHORING a fact must not be shown the
// vocabulary, because reuse-before-minting distorts what gets written. Backfill
// is the one place that concern is knowingly traded away — the fact already
// exists and its claim is fixed, so the only thing showing vocabulary can bias
// is which existing name gets reused, which is the point.

// maxBackfillFacts is an LLM-SPEND BUDGET: facts offered for backfill per
// session. Oldest-first, so a corpus sweeps its backlog in a stable order
// rather than re-offering the same head every session.
const maxBackfillFacts = 8

// backfillVocabularyShown is a PROMPT-SIZE BUDGET: how many canonical motifs
// the prompt lists. §6 says ~20; enough that a reusable name is likely present,
// few enough that the list can be read rather than skimmed.
const backfillVocabularyShown = 20

// backfillNeighboursScanned is a PROMPT-SIZE BUDGET on the hint generator's
// input: how many title-nearest facts are consulted to find cluster-relevant
// motifs. Larger than the shown count because neighbours often share motifs.
const backfillNeighboursScanned = 30

// subtractionResidue is the §11 aspect signal: the fact's own title tokens
// MINUS its subject tokens (entities ∪ domain ∪ path).
//
// What remains is, by construction, the words the author used that are NOT
// about the subject — which is what a motif names. Measured decorrelated from
// the body embedding (r ≈ 0.3) and blind-confirmed to surface real mechanism
// analogies.
//
// It failed as a PRODUCT — yield hostage to authoring accidents, 98% hapax
// slugs, matched random on one corpus — and survives exactly here, as a hint to
// a reader who can ignore it. That distinction is the whole reason it is in a
// prompt and not in a scorer.
func subtractionResidue(t store.BackfillTarget) []string {
	subject := map[string]struct{}{}
	add := func(s string) {
		for _, tok := range textnorm.Tokens(textnorm.Canonicalize(s)) {
			subject[tok] = struct{}{}
		}
	}
	for _, e := range t.Entities {
		add(e)
	}
	for _, d := range t.Domain {
		add(d)
	}
	add(strings.ReplaceAll(t.Path, "/", " "))

	seen := map[string]struct{}{}
	var out []string
	for _, t := range textnorm.Tokens(textnorm.Canonicalize(t.Title)) {
		if _, isSubject := subject[t]; isSubject {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// backfillMotifHint is one vocabulary entry offered to the backfill agent.
type backfillMotifHint struct {
	Motif      string `json:"motif"`
	DF         int    `json:"df"`
	Definition string `json:"definition,omitempty"`
}

// backfillItem is one fact offered for backfill.
type backfillItem struct {
	Path string `json:"path"`
	// FactID is the VERSION this item was planned against. It rides the payload
	// for the same reason the define pass carries its cluster key: the payload
	// round-trips through the work item's JSON, and the answer has to be
	// routable back to the thing it was about.
	//
	// `facts` rows are immutable and unique on (path, blob_hash), so this id IS
	// the content address. Comparing it at apply time is what stops a judgement
	// — or an assignment — landing on a version the agent never saw, which an
	// ordinary learn/update between planning and answering will produce.
	FactID int64 `json:"fact_id"`
	// Residue is the §11 subtraction residue — the fact's own title words that
	// are not about its subject.
	Residue []string `json:"residue,omitempty"`
}

// backfillFactsFor builds the offered-facts half of the payload.
//
// One derivation, shared by the planner and by anything that needs to know what
// a session WOULD have offered. Two constructions that happen to agree are a
// test asserting an arrangement; one construction used twice is the rule.
func backfillFactsFor(targets []store.BackfillTarget) []backfillItem {
	out := make([]backfillItem, 0, len(targets))
	for _, t := range targets {
		out = append(out, backfillItem{
			Path:    t.Path,
			FactID:  t.FactID,
			Residue: subtractionResidue(t),
		})
	}
	return out
}

// backfillPayload is the work item's facts field.
type backfillPayload struct {
	Facts []backfillItem `json:"facts"`
	// Vocabulary is the corpus's existing motifs, with df and definitions. THE
	// one place vocabulary appears in a prompt.
	Vocabulary []backfillMotifHint `json:"vocabulary"`
}

// buildBackfillHints selects the vocabulary to show for a set of target facts.
//
// Cluster-relevant, not merely frequent: the motifs come from the targets' own
// TITLE-NEAREST neighbours on the Phase-0 abstraction axis, so a fact about
// caching is shown caching-adjacent mechanisms rather than the corpus's global
// top twenty. That axis was built for the restatement shortlist and is reused
// here exactly as the roadmap's space budget anticipated.
//
// Restricted to df >= 2: a hapax motif is one author's coinage that nothing has
// reused, and offering it as a candidate would spread a name the corpus has not
// endorsed. The band floor is the same one bridging uses.
func buildBackfillHints(ctx context.Context, d Deps, branch string, targets []store.BackfillTarget) []backfillMotifHint {
	dfByCluster := map[string]int{}
	nameByCluster := map[string]string{}

	for _, t := range targets {
		neighbours, err := d.Abstraction.TopTitleNeighbours(ctx, branch, t.FactID, backfillNeighboursScanned)
		if err != nil {
			continue // a fact with no title vector yet contributes nothing
		}
		for _, n := range neighbours {
			rec, err := d.Search.GetByPath(ctx, branch, n.Path)
			if err != nil || rec == nil {
				continue
			}
			for _, m := range rec.Motifs {
				key, err := d.Motifs.ClusterKey(ctx, branch, m)
				if err != nil {
					continue
				}
				if _, seen := dfByCluster[key]; seen {
					continue
				}
				canonical, err := d.Motifs.CanonicalID(ctx, branch, m)
				if err != nil {
					continue
				}
				df, err := d.Search.TokenDF(ctx, branch, canonical, "motif")
				if err != nil || df < 2 {
					// Hapax: one author's coinage, unreused. Offering it would
					// spread a name the corpus has not endorsed.
					continue
				}
				dfByCluster[key] = df
				nameByCluster[key] = canonical
			}
		}
	}

	hints := make([]backfillMotifHint, 0, len(dfByCluster))
	for key, df := range dfByCluster {
		h := backfillMotifHint{Motif: nameByCluster[key], DF: df}
		if def, ok, err := d.Motifs.Definition(ctx, branch, key); err == nil && ok {
			h.Definition = def
		}
		hints = append(hints, h)
	}
	// Most-used first, ties by name: deterministic, and it puts the corpus's
	// established vocabulary at the top where a skimming reader sees it.
	sort.Slice(hints, func(i, j int) bool {
		if hints[i].DF != hints[j].DF {
			return hints[i].DF > hints[j].DF
		}
		return hints[i].Motif < hints[j].Motif
	})
	if len(hints) > backfillVocabularyShown {
		hints = hints[:backfillVocabularyShown]
	}
	return hints
}

// backfillHealth reports the pass's coverage picture. Nothing branches on it.
type backfillHealth struct {
	WithMotifs int
	TotalFacts int
	Offered    int
	Vocabulary int
}

func recordBackfillHealth(sess *store.PipelineSession, h backfillHealth) {
	if sess == nil || h.TotalFacts == 0 {
		return
	}
	pct := 100 * float64(h.WithMotifs) / float64(h.TotalFacts)
	sess.Health = append(sess.Health, fmt.Sprintf(
		"motif backfill: coverage %.0f%% (%d/%d authored facts), %d offered this session, %d vocabulary shown",
		pct, h.WithMotifs, h.TotalFacts, h.Offered, h.Vocabulary))
}

// planMotifBackfillWork enqueues at most one backfill item per session.
func planMotifBackfillWork(ctx context.Context, d Deps, sess *store.PipelineSession, branch string) error {
	with, total, err := d.Motifs.MotifCoverage(ctx, branch)
	if err != nil {
		return nil // an addition to review; degrade rather than fail
	}
	health := backfillHealth{WithMotifs: with, TotalFacts: total}
	defer func() { recordBackfillHealth(sess, health) }()

	targets, err := d.Motifs.LiveFactsWithoutMotifs(ctx, branch, maxBackfillFacts)
	if err != nil || len(targets) == 0 {
		return nil
	}

	payload := backfillPayload{
		Facts:      backfillFactsFor(targets),
		Vocabulary: buildBackfillHints(ctx, d, branch, targets),
	}
	health.Offered = len(payload.Facts)
	health.Vocabulary = len(payload.Vocabulary)

	blob, err := json.Marshal(payload)
	if err != nil {
		return wrapf(reviewTool, err, "motif backfill: marshal payload")
	}
	return d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   motifBackfillStepType,
		ClusterKey: "motif-backfill",
		FactsJSON:  string(blob),
		Priority:   motifBackfillPriority,
	})
}

// motifBackfillStepType is the work-item step type for the backfill pass.
const motifBackfillStepType = "motif_backfill"

// motifBackfillPriority sits below the definition pass: backfill wants a
// resolved, defined vocabulary to offer, so it runs after both.
const motifBackfillPriority = 1.0

// motifBackfillResponseSchema is what the backfill agent must return.
const motifBackfillResponseSchema = `{
  "type": "object",
  "properties": {
    "assignments": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "The path, exactly as given."},
          "motifs": {"type": "array", "items": {"type": "string"}, "description": "At most 3. An empty array is correct and common — many facts instantiate no general regularity."}
        },
        "required": ["path", "motifs"]
      }
    }
  },
  "required": ["assignments"]
}`

type motifAssignment struct {
	Path   string   `json:"path"`
	Motifs []string `json:"motifs"`
}

type motifBackfillResult struct {
	Assignments []motifAssignment `json:"assignments"`
}

// parseMotifBackfillResponse decodes and probes the envelope (invariant
// 51d85fcd). Unlike the `motifs` field INSIDE a derived-writer schema — where
// absence legitimately means "none" — `assignments` is an envelope key, and its
// absence means the response arrived under a different name and would apply as
// a silent no-op.
func parseMotifBackfillResponse(raw string) (motifBackfillResult, error) {
	var out motifBackfillResult
	text := extractJSON(raw)
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return out, fmt.Errorf("parse motif backfill response: %w", err)
	}
	if err := requireResponseKey(text, "assignments"); err != nil {
		return out, err
	}
	return out, nil
}

// validateMotifBackfill checks the response against what was offered.
//
// A path the item never offered is refused: backfill REWRITES facts, so an
// invented path would put motifs on a fact nobody asked about.
func validateMotifBackfill(res motifBackfillResult, offered backfillPayload) error {
	valid := map[string]struct{}{}
	for _, f := range offered.Facts {
		valid[f.Path] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, a := range res.Assignments {
		if _, ok := valid[a.Path]; !ok {
			return fmt.Errorf("assignment names %q, which was not offered in this item", a.Path)
		}
		if _, dup := seen[a.Path]; dup {
			return fmt.Errorf("assignment for %q appears more than once", a.Path)
		}
		seen[a.Path] = struct{}{}
	}
	return nil
}

// applyMotifBackfill writes the assigned motifs onto their facts.
//
// Every write goes through SerializeFact (MN4), so the count cap, the kebab
// shape and the silent subject strip apply here exactly as they do to a
// hand-authored fact — backfill invents no second gate, and a motif the agent
// proposed that restates its fact's subject is dropped without ceremony.
//
// A fact that has GAINED motifs since the item was rendered is skipped rather
// than overwritten: something else — a human, an update — has answered the
// question this item was asking, and the fresher answer wins.
//
// EVERY branch binds to the version that was OFFERED. The answer is about the
// content the agent read, and between rendering and answering an ordinary
// learn/update can replace it — so a fact whose live version has moved is
// skipped whole: no write, and no judgement. The edited version is a new,
// never-judged row and returns to the backlog on its own, which is the
// content-addressing the drain record was always supposed to rest on. The
// "gained motifs" check above is NOT this guard and does not subsume it: an
// edit need not add a motif.
func applyMotifBackfill(ctx context.Context, d Deps, branch string, res motifBackfillResult, offered backfillPayload) error {
	offeredID := make(map[string]int64, len(offered.Facts))
	for _, f := range offered.Facts {
		offeredID[f.Path] = f.FactID
	}
	paths := make([]string, 0, len(res.Assignments))
	for _, a := range res.Assignments {
		paths = append(paths, a.Path)
	}
	liveID, err := d.Motifs.LiveFactIDs(ctx, branch, paths)
	if err != nil {
		return fmt.Errorf("motif backfill: resolve live versions: %w", err)
	}

	// "No regularity here" is an ANSWER, and it is recorded. Without the record
	// the pass cannot tell it from "not yet asked", and re-offers the fact every
	// session for the life of the corpus. Recorded against the fact ID that was
	// JUDGED, never against whatever is live now.
	var judgedEmpty []int64
	for _, a := range res.Assignments {
		want, offeredHere := offeredID[a.Path]
		if !offeredHere || want == 0 {
			// Not in this item's payload, or an item planned before the id was
			// carried. Neither is a version this pass can vouch for.
			log.Debug().Str("path", a.Path).
				Msg("motif backfill: assignment has no offered version to bind to; skipping")
			continue
		}
		if liveID[a.Path] != want {
			log.Debug().Str("path", a.Path).Int64("offered", want).Int64("live", liveID[a.Path]).
				Msg("motif backfill: fact changed since the item was rendered; skipping")
			continue
		}
		if len(a.Motifs) == 0 {
			judgedEmpty = append(judgedEmpty, want)
			continue
		}
		rec, err := d.Search.GetByPath(ctx, branch, a.Path)
		if err != nil || rec == nil {
			continue
		}
		if len(rec.Motifs) > 0 {
			log.Debug().Str("path", a.Path).
				Msg("motif backfill: fact gained motifs since the item was rendered; skipping")
			continue
		}
		// The fact's FULL SOURCE, not the indexed body. FactWithBody.Body is
		// the content BELOW the frontmatter, so parsing it would yield a fact
		// with no domain, no entities and no title — and serializing that back
		// would replace a real fact with a husk. Backfill is a write path, so
		// this mistake destroys data rather than merely producing a bad hint.
		src, err := d.Facts.ReadFact(ctx, branch, a.Path, nil)
		if err != nil {
			log.Warn().Err(err).Str("path", a.Path).Msg("motif backfill: unreadable fact; skipping")
			continue
		}
		parsed, err := fact.ParseFact(a.Path, src.Content)
		if err != nil {
			log.Warn().Err(err).Str("path", a.Path).Msg("motif backfill: unparseable fact; skipping")
			continue
		}
		parsed.Motifs = a.Motifs
		content, err := fact.SerializeFact(parsed)
		if err != nil {
			// The single gate refused it — an over-cap or malformed list. The
			// fact keeps no motifs and is offered again next session, which is
			// the same outcome as the agent having returned none.
			log.Warn().Err(err).Str("path", a.Path).Msg("motif backfill: rejected by the write gate")
			continue
		}
		// Did the SILENT half of the gate take the whole answer?
		//
		// SerializeFact validates and then strips subject motifs without
		// reporting it, so an answer made entirely of subject-restatements
		// serializes CLEANLY to a fact with no motifs. That is not the refusal
		// case above and must not be treated like one: a refused NAME can be
		// fixed by naming it better next time, while a subject restatement has
		// nothing to fix — the same content, offered again with the same hints,
		// draws the same answer forever. The agent judged this fact and only
		// restatements came back, which is "no regularity here".
		//
		// Read off what the write WOULD STORE rather than re-deriving the rule:
		// parsing SerializeFact's own output cannot drift from it, and MN4 keeps
		// the strip defined in exactly one place.
		stored, perr := fact.ParseFact(a.Path, content)
		if perr == nil && len(stored.Motifs) == 0 {
			log.Debug().Str("path", a.Path).Strs("proposed", a.Motifs).
				Msg("motif backfill: the subject strip absorbed the whole answer; judged empty")
			judgedEmpty = append(judgedEmpty, want)
			// Nothing to write — the fact is unchanged. Writing anyway would
			// mint a new version, and the judgement below would then name a
			// version that is no longer live.
			continue
		}
		if _, err := d.Facts.WriteFact(ctx, branch, a.Path, content,
			"motif backfill: assign motifs", "update"); err != nil {
			log.Warn().Err(err).Str("path", a.Path).Msg("motif backfill: write failed")
		}
	}
	// Recorded AFTER the writes: a fact that gained a motif leaves the backlog
	// by carrying one, and only the genuinely empty answers need the record.
	return d.Motifs.RecordBackfillJudgedEmpty(ctx, branch, judgedEmpty)
}

// RenderMotifBackfillWorkItem renders the backfill prompt.
//
// This is the ONE prompt whose PAYLOAD legitimately carries corpus vocabulary
// (MN1, designer ruling Q8). The template itself carries none — the vocabulary
// rides in the work item's facts field like every other payload — so the MN1
// enumeration over templates stays a clean check either way.
func RenderMotifBackfillWorkItem(factsJSON string) (*WorkItemContent, error) {
	prompt, err := RenderTemplate("motif_backfill", "user", PromptData{})
	if err != nil {
		return nil, fmt.Errorf("render motif backfill work item: %w", err)
	}
	return &WorkItemContent{
		Prompt:         prompt,
		ResponseSchema: motifBackfillResponseSchema,
		// The facts AND the vocabulary. This payload is the one place corpus
		// vocabulary legitimately reaches a model (MN1, Q8); shipping it empty
		// meant the backfill agent was asked to prefer existing names while
		// being shown none.
		Facts: factsJSON,
	}, nil
}

// sharedClusterMotifs renders the motifs carried by TWO OR MORE facts in a
// cluster, for the §6 distill enrichment line.
//
// Shared, not merely present: a motif on one member says something about that
// member, while a motif on several says something about the group — which is
// what the synthesized claim is about. A list of every member's motifs would
// be longer and say less.
//
// Deliberately mechanical and alias-blind. It compares the strings as written,
// because it runs while rendering a prompt and has no branch handy to resolve
// through; the cost of missing two spellings of one mechanism is a slightly
// thinner hint, and the alternative is threading a store read into a pure
// render function for a line that is free context either way.
func sharedClusterMotifs(facts []factForLLM) string {
	count := map[string]int{}
	for _, f := range facts {
		seen := map[string]struct{}{}
		for _, m := range f.Motifs {
			if _, dup := seen[m]; dup {
				continue // one fact is one carrier, however it repeats itself
			}
			seen[m] = struct{}{}
			count[m]++
		}
	}
	var shared []string
	for m, n := range count {
		if n >= 2 {
			shared = append(shared, m)
		}
	}
	if len(shared) == 0 {
		return ""
	}
	sort.Strings(shared)
	return strings.Join(shared, ", ")
}
