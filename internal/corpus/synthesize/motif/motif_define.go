package motif

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/vocab"

	"github.com/rs/zerolog/log"
)

// Blind definition authoring (blueprint §3.2): one glossary sentence per motif
// cluster, written from the NAME ALONE.
//
// The blindness is the design. A writer shown the carrier facts would describe
// what those facts are about; a writer shown only the name has to describe the
// mechanism, which is the only kind of sentence that still fits the next fact
// about some entirely different system. It also makes the generic register
// achievable rather than merely requested — a writer who never saw a product
// name cannot put one in.

// DefineStepType is the work-item step type for the definition pass.
const DefineStepType = "motif_define"

// motifDefinePriority sits just below the alias judge: definitions are written
// over clusters, so resolving the vocabulary first avoids authoring a sentence
// for a cluster that is about to absorb another.
const motifDefinePriority = 1.1

// maxDefinitionsPerSession is an LLM-SPEND BUDGET: how many definitions one
// review session will author. Targets arrive most-frequent-first, so a bounded
// pass spends on the vocabulary the corpus actually leans on, and the rest is
// simply still queued next session — the queue is derived, so nothing has to
// remember where it got to.
const maxDefinitionsPerSession = 24

// DefineItem is one name as the authoring pass sees it.
//
// Name and Current only. There is deliberately no carrier field: see the file
// comment, and the prompt, which states the reason rather than the rule.
type DefineItem struct {
	Name string `json:"name"`
	// Current is the definition standing for this cluster, shown on a REFRESH
	// so the model revises rather than restarts (designer ruling): definition
	// text feeds the name+def matching embeddings, so drift on every refresh
	// would wobble downstream operating points. Blindness to CARRIERS is
	// preserved either way, which is the property that matters.
	Current string `json:"current,omitempty"`
	// clusterKey routes the response back to a cluster whose representative may
	// have flipped meanwhile. It is not one of the fields the prompt asks the
	// model to reason about — though since C2 the whole payload IS served, so
	// the model does see it. (An earlier comment here said it was not sent at
	// all; that was true only while the renderers shipped no payload.)
	clusterKey string
	// members is the cluster's membership when this item was PLANNED. Like
	// clusterKey it rides the served payload; it is what
	// the stored definition is stamped with, so a judge merge applied later in
	// the same session cannot mark a pre-merge definition as current for a
	// post-merge cluster.
	members string
	// membersKnown says members came from a DefinitionTarget. An unresolved
	// cluster legitimately HAS no members, and an empty string cannot tell that
	// apart from an item planned before this field existed — which is the
	// conflation that silently reverted the stamp to read-at-write-time.
	membersKnown bool
}

const DefineResponseSchema = `{
  "type": "object",
  "properties": {
    "definitions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "The name, exactly as given."},
          "definition": {"type": "string", "description": "One sentence describing the mechanism, or an empty string if the name cannot be defined from itself."}
        },
        "required": ["name", "definition"]
      }
    }
  },
  "required": ["definitions"]
}`

type motifDefinition struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type DefineResult struct {
	Definitions []motifDefinition `json:"definitions"`
}

// ParseDefineResponse decodes and probes the envelope (invariant
// 51d85fcd — the schema's `required` list is inert without a probe on the raw
// object, and a response under the wrong key would apply as a silent no-op).
func ParseDefineResponse(raw string) (DefineResult, error) {
	var out DefineResult
	text := step.ExtractJSON(raw)
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return out, fmt.Errorf("parse motif define response: %w", err)
	}
	if err := step.RequireResponseKey(text, "definitions"); err != nil {
		return out, err
	}
	return out, nil
}

// ValidateDefinitions checks the response against what was asked for.
//
// A definition for a name this item never offered is refused: it would be
// stored against whatever cluster happened to match, and a definition is used
// to decide whether unrelated facts describe the same mechanism.
//
// An EMPTY definition is legitimate — the prompt asks for one rather than a
// guess when a name cannot be defined from itself. It is simply not stored, so
// the cluster stays queued.
func ValidateDefinitions(res DefineResult, offered []DefineItem) error {
	valid := map[string]struct{}{}
	for _, it := range offered {
		valid[it.Name] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, d := range res.Definitions {
		if _, ok := valid[d.Name]; !ok {
			return fmt.Errorf("definition names %q, which was not offered in this item", d.Name)
		}
		if _, dup := seen[d.Name]; dup {
			return fmt.Errorf("definition for %q appears more than once", d.Name)
		}
		seen[d.Name] = struct{}{}
	}
	return nil
}

// ApplyDefinitions stores the authored sentences.
//
// Routed by CLUSTER KEY, not by name. The representative spelling can flip
// between the item being rendered and answered — that is exactly what
// canonical_id does — and storing by name would then write the sentence
// against a cluster nobody asked about.
func ApplyDefinitions(ctx context.Context, d step.Deps, branch string, res DefineResult, offered []DefineItem) error {
	type route struct {
		key   string
		stamp vocab.DefinitionStamp
	}
	keyByName := make(map[string]route, len(offered))
	for _, it := range offered {
		keyByName[it.Name] = route{
			key: it.clusterKey,
			// Known tracks whether THIS ITEM carried a membership, so an item
			// planned by a build that did not yet stamp one falls back to
			// reading current membership instead of stamping a false empty.
			stamp: vocab.DefinitionStamp{Members: it.members, Known: it.membersKnown},
		}
	}
	for _, def := range res.Definitions {
		text := strings.TrimSpace(def.Definition)
		if text == "" {
			// "I cannot define this from the name alone" is a legitimate
			// answer, and a better one than a guess: the sentence would be used
			// to decide whether unrelated facts describe the same mechanism.
			// Storing nothing leaves the cluster queued for a later pass.
			continue
		}
		r := keyByName[def.Name]
		if r.key == "" {
			continue // validated above; belt and braces
		}
		if err := d.Motifs.PutDefinition(ctx, branch, r.key, text, r.stamp); err != nil {
			log.Warn().Err(err).Str("motif", def.Name).
				Msg("motif define: definition not stored; the cluster stays queued")
		}
	}
	return nil
}

// motifVocabularyHealthLines renders the §3.3 metrics for the session's health
// output. Reported, never read by a branch (MN6): these are the numbers a
// designer looks at to decide whether the axis is alive, and §3.4's kill switch
// is a human decision, not an automatic one.
func motifVocabularyHealthLines(h vocab.MotifVocabularyHealth) []string {
	if h.Clusters == 0 {
		return nil
	}
	// BOTH POPULATIONS, BOTH LABELLED (knomit#116). The raw figure counts every
	// authored carrier; the activation floor reads only EPISTEMIC ones, and the
	// two diverge by up to 5x on measured corpora. The old line's "authored
	// facts only" qualifier filters ORIGIN, not KIND — a reader takes it as
	// naming the population, and it names a different axis.
	//
	// Labelling, not redefinition: raw recurrence keeps its meaning, because it
	// is the honest answer to "is this vocabulary being reused at all". The
	// epistemic count answers a different question — "how close is bridging to
	// activating" — and only that one was ever unavailable.
	return []string{
		fmt.Sprintf(
			"motif vocabulary: %d clusters, recurrence %.0f%% (%d recur over authored "+
				"carriers; %d recur over EPISTEMIC carriers — the population the "+
				"activation floor reads), mint-to-link %.2f (%d mints / %d links)",
			h.Clusters, 100*h.RecurrenceRate(), h.Recurring, h.EpistemicRecurring,
			h.MintToLinkRatio(), h.Mints, h.Links),

		// ONE SENTENCE, ITS OWN LINE, REMOVED BY #127's FIX. df counts
		// CARRIERS, and duplicate records of one event inflate it — measured
		// live at df 4 resting on one event. Labelling the population correctly
		// does not make the count correct, so this says so rather than shipping
		// a more precise wrong number.
		//
		// Deliberately self-contained and last: #127's PR deletes this line, and
		// nothing above depends on its presence or its position.
		"motif vocabulary caveat: df counts CARRIERS, not distinct claims — " +
			"duplicate records of one event inflate every figure above (#127); " +
			"treat them as upper bounds until that lands.",
	}
}

// motifDefineHealth reports what the pass did. Nothing branches on it.
type motifDefineHealth struct {
	Queued  int
	Offered int
}

func recordMotifDefineHealth(sess *model.PipelineSession, h motifDefineHealth) {
	if sess == nil {
		return
	}
	if h.Queued == 0 {
		return
	}
	// Appends, like every health recorder — see
	// TestHealthRecorders_NeverDestroyExistingLines for why that is a rule
	// rather than a convention.
	sess.Health = append(sess.Health, fmt.Sprintf(
		"motif definitions: %d clusters need one, %d authored this session",
		h.Queued, h.Offered))
}

// PlanDefineWork enqueues at most one definition item per session.
func PlanDefineWork(ctx context.Context, d step.Deps, sess *model.PipelineSession, branch string) error {
	targets, err := d.Motifs.ClustersNeedingDefinition(ctx, branch)
	if err != nil {
		return nil // an addition to review; degrade rather than fail the session
	}
	health := motifDefineHealth{Queued: len(targets)}
	defer func() { recordMotifDefineHealth(sess, health) }()

	// §3.3's vocabulary metrics ride the same pass — they are computed from the
	// same resolved vocabulary, and reporting them anywhere else would mean a
	// second read of it.
	if vh, vErr := d.Motifs.VocabularyHealth(ctx, branch); vErr == nil && sess != nil {
		sess.Health = append(sess.Health, motifVocabularyHealthLines(vh)...)
	}
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > maxDefinitionsPerSession {
		targets = targets[:maxDefinitionsPerSession]
	}
	items := make([]DefineItem, 0, len(targets))
	for _, t := range targets {
		items = append(items, DefineItem{
			Name:       t.Name,
			Current:    t.Interim,
			clusterKey: t.ClusterKey,
			members:    t.Members,
			// Every target came from ClustersNeedingDefinition, which always
			// carries the membership it selected against — empty included.
			membersKnown: true,
		})
	}
	health.Offered = len(items)
	payload, err := json.Marshal(motifDefinePayload(items))
	if err != nil {
		return step.Wrapf(step.ReviewTool, err, "motif define: marshal names")
	}
	return d.Pipeline.InsertPipelineWorkItem(ctx, model.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   DefineStepType,
		ClusterKey: "motif-define",
		FactsJSON:  string(payload),
		Priority:   motifDefinePriority,
	})
}

// motifDefinePayloadEntry is the on-the-wire form, carrying the routing fields
// alongside the ones the prompt asks about.
//
// "On-the-wire" is literal: this payload is the work item's facts field, which
// since C2 is served to the model verbatim. The routing fields are not secret,
// they are simply not what the definer is asked to reason about.
//
// The key is a separate exported field rather than an unexported one on
// DefineItem because the payload round-trips through the work item's
// JSON: an unexported field would marshal away, and ApplyDefinitions
// would then have no cluster to store against — silently writing nothing on
// every refresh.
type motifDefinePayloadEntry struct {
	Name       string `json:"name"`
	Current    string `json:"current,omitempty"`
	ClusterKey string `json:"cluster_key"`
	// Members rides the payload for the same reason ClusterKey does: an
	// unexported field would marshal away, and the stamp would silently fall
	// back to current membership — the exact race this carries it to avoid.
	Members string `json:"members"`
	// MembersKnown rides the payload so the distinction survives the work
	// item's JSON. Without it, an item planned by an older build and an item
	// for an unresolved cluster decode identically.
	MembersKnown bool `json:"members_known"`
}

func motifDefinePayload(items []DefineItem) []motifDefinePayloadEntry {
	out := make([]motifDefinePayloadEntry, len(items))
	for i, it := range items {
		out[i] = motifDefinePayloadEntry{
			Name: it.Name, Current: it.Current,
			ClusterKey: it.clusterKey, Members: it.members,
			MembersKnown: it.membersKnown,
		}
	}
	return out
}

func DefineItemsFromPayload(raw string) ([]DefineItem, error) {
	var entries []motifDefinePayloadEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	out := make([]DefineItem, len(entries))
	for i, e := range entries {
		out[i] = DefineItem{
			Name: e.Name, Current: e.Current,
			clusterKey: e.ClusterKey, members: e.Members,
			membersKnown: e.MembersKnown,
		}
	}
	return out, nil
}

// VocabularySection renders the §3.3 metrics for the reflect prompt, or
// "" when the corpus has no motif vocabulary.
//
// Empty rather than zeroes: a reflection prompt carrying "0 clusters,
// recurrence 0%" invites the model to reason about a mechanism this corpus is
// not using, and the template omits the whole section when this is empty.
//
// Reflect is the right home for these numbers. It is the pass that reasons
// about the corpus's own habits, and recurrence is exactly such a habit — the
// question "are we naming the same mechanisms as each other" is a methodology
// question, not a fact-level one.
func VocabularySection(ctx context.Context, d step.Deps, branch string) string {
	if d.Motifs == nil {
		return ""
	}
	h, err := d.Motifs.VocabularyHealth(ctx, branch)
	if err != nil || h.Clusters == 0 {
		return ""
	}
	return fmt.Sprintf(
		"%d motif clusters across authored facts. Recurrence %.0f%% (%d of %d carried by more than one fact). Mint-to-link %.2f (%d names minted, %d reuses).",
		h.Clusters, 100*h.RecurrenceRate(), h.Recurring, h.Clusters,
		h.MintToLinkRatio(), h.Mints, h.Links)
}
