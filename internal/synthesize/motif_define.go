package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
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

// motifDefineStepType is the work-item step type for the definition pass.
const motifDefineStepType = "motif_define"

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

// motifDefineItem is one name as the authoring pass sees it.
//
// Name and Current only. There is deliberately no carrier field: see the file
// comment, and the prompt, which states the reason rather than the rule.
type motifDefineItem struct {
	Name string `json:"name"`
	// Current is the definition standing for this cluster, shown on a REFRESH
	// so the model revises rather than restarts (designer ruling): definition
	// text feeds the name+def matching embeddings, so drift on every refresh
	// would wobble downstream operating points. Blindness to CARRIERS is
	// preserved either way, which is the property that matters.
	Current string `json:"current,omitempty"`
	// clusterKey is not sent to the model — it is how the response is routed
	// back to a cluster whose representative may have flipped meanwhile.
	clusterKey string
}

const motifDefineResponseSchema = `{
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

type motifDefineResult struct {
	Definitions []motifDefinition `json:"definitions"`
}

// parseMotifDefineResponse decodes and probes the envelope (invariant
// 51d85fcd — the schema's `required` list is inert without a probe on the raw
// object, and a response under the wrong key would apply as a silent no-op).
func parseMotifDefineResponse(raw string) (motifDefineResult, error) {
	var out motifDefineResult
	text := extractJSON(raw)
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return out, fmt.Errorf("parse motif define response: %w", err)
	}
	if err := requireResponseKey(text, "definitions"); err != nil {
		return out, err
	}
	return out, nil
}

// validateMotifDefinitions checks the response against what was asked for.
//
// A definition for a name this item never offered is refused: it would be
// stored against whatever cluster happened to match, and a definition is used
// to decide whether unrelated facts describe the same mechanism.
//
// An EMPTY definition is legitimate — the prompt asks for one rather than a
// guess when a name cannot be defined from itself. It is simply not stored, so
// the cluster stays queued.
func validateMotifDefinitions(res motifDefineResult, offered []motifDefineItem) error {
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

// applyMotifDefinitions stores the authored sentences.
//
// Routed by CLUSTER KEY, not by name. The representative spelling can flip
// between the item being rendered and answered — that is exactly what
// canonical_id does — and storing by name would then write the sentence
// against a cluster nobody asked about.
func applyMotifDefinitions(ctx context.Context, d Deps, branch string, res motifDefineResult, offered []motifDefineItem) error {
	keyByName := make(map[string]string, len(offered))
	for _, it := range offered {
		keyByName[it.Name] = it.clusterKey
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
		key := keyByName[def.Name]
		if key == "" {
			continue // validated above; belt and braces
		}
		if err := d.Motifs.PutDefinition(ctx, branch, key, text); err != nil {
			log.Warn().Err(err).Str("motif", def.Name).
				Msg("motif define: definition not stored; the cluster stays queued")
		}
	}
	return nil
}

// motifDefineHealth reports what the pass did. Nothing branches on it.
type motifDefineHealth struct {
	Queued  int
	Offered int
}

func recordMotifDefineHealth(sess *store.PipelineSession, h motifDefineHealth) {
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

// planMotifDefineWork enqueues at most one definition item per session.
func planMotifDefineWork(ctx context.Context, d Deps, sess *store.PipelineSession, branch string) error {
	targets, err := d.Motifs.ClustersNeedingDefinition(ctx, branch)
	if err != nil {
		return nil // an addition to review; degrade rather than fail the session
	}
	health := motifDefineHealth{Queued: len(targets)}
	defer func() { recordMotifDefineHealth(sess, health) }()
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > maxDefinitionsPerSession {
		targets = targets[:maxDefinitionsPerSession]
	}
	items := make([]motifDefineItem, 0, len(targets))
	for _, t := range targets {
		items = append(items, motifDefineItem{
			Name:       t.Name,
			Current:    t.Interim,
			clusterKey: t.ClusterKey,
		})
	}
	health.Offered = len(items)
	payload, err := json.Marshal(motifDefinePayload(items))
	if err != nil {
		return wrapf(reviewTool, err, "motif define: marshal names")
	}
	return d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   motifDefineStepType,
		ClusterKey: "motif-define",
		FactsJSON:  string(payload),
		Priority:   motifDefinePriority,
	})
}

// motifDefinePayloadEntry is the on-the-wire form, carrying the cluster key
// alongside the model-visible fields so the answer can be routed back.
//
// The key is a separate exported field rather than an unexported one on
// motifDefineItem because the payload round-trips through the work item's
// JSON: an unexported field would marshal away, and applyMotifDefinitions
// would then have no cluster to store against — silently writing nothing on
// every refresh.
type motifDefinePayloadEntry struct {
	Name       string `json:"name"`
	Current    string `json:"current,omitempty"`
	ClusterKey string `json:"cluster_key"`
}

func motifDefinePayload(items []motifDefineItem) []motifDefinePayloadEntry {
	out := make([]motifDefinePayloadEntry, len(items))
	for i, it := range items {
		out[i] = motifDefinePayloadEntry{Name: it.Name, Current: it.Current, ClusterKey: it.clusterKey}
	}
	return out
}

func motifDefineItemsFromPayload(raw string) ([]motifDefineItem, error) {
	var entries []motifDefinePayloadEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	out := make([]motifDefineItem, len(entries))
	for i, e := range entries {
		out[i] = motifDefineItem{Name: e.Name, Current: e.Current, clusterKey: e.ClusterKey}
	}
	return out, nil
}
