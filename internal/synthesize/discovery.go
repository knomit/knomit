// Package synthesize — discovery integrates the bridge engine, the discover
// work-item shape, the agent prompt, and the ingest path with its gates.
//
// Plan 03 Tasks 4 (forward — review/synthesis) and 5 (backward — hypothesize/
// keystone), plus Plan 04 Tasks 1-4 (verification gates).
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// DiscoverDirection distinguishes the two emergent-fact shapes the discovery
// engine produces. Forward = deductive consequence of the seed cluster
// (type=synthesis). Backward = abductive keystone that would entail the seed
// cluster (type=hypothesis). See Plan 03 D3.
type DiscoverDirection string

const (
	DiscoverForward  DiscoverDirection = "forward"
	DiscoverBackward DiscoverDirection = "backward"
)

// DiscoverWorkPayload is the FactsJSON payload carried by a discover work
// item. It is small and self-contained so the dispatcher can rehydrate
// everything the prompt builder needs without re-running the bridge engine.
type DiscoverWorkPayload struct {
	Direction  DiscoverDirection `json:"direction"`
	Bridge     BridgeSeedSet     `json:"bridge"`
	ScopeLabel string            `json:"scope_label,omitempty"`
	// Lane is set for motif bridges only (§4). It is carried EXPLICITLY rather
	// than re-derived from Kind+Direction so that the prompt renderer and the
	// health reporter read the same value the builder decided, and so a future
	// bridge kind with two lanes does not silently inherit this one's mapping.
	Lane BridgeLane `json:"lane,omitempty"`
}

// DiscoverResponse is the LLM JSON contract for one discover work item. The
// agent returns zero or more proposed facts. Empty is the expected outcome —
// the strict default-skip instructions are reinforced by the prompt (Plan 04
// Task 1). Surviving proposals are then run through confidence/dedup/blast
// gates before any write.
type DiscoverResponse struct {
	Proposals []DiscoveredFact `json:"proposals"`
	// Reinforcements is discovery's THIRD outcome (GATE rider 3): facts the
	// corpus already holds that these seeds independently re-derive. Absent is
	// the common case and is not an error.
	Reinforcements []FactReinforcement `json:"reinforcements"`
}

// FactReinforcement records that the seeds of this bridge are an INDEPENDENT
// derivation of a fact the corpus already states.
//
// Reason is REQUIRED, and it is not decoration: the equivalence claim gets the
// same discipline the alias judge's merges get, because an equivalence nobody
// could justify in a sentence is the hallucinated one, and an over-merge is
// invisible everywhere downstream. Absence of a reason is a rejection, not a
// blank field.
type FactReinforcement struct {
	// Path is the existing corpus fact being reinforced.
	Path string `json:"path"`
	// Reason is the agent's one sentence on why the two claims are the same.
	Reason string `json:"reason"`
	// Refs are the seeds to join the fact's derivation paths.
	Refs flexStrings `json:"refs"`
}

// DiscoveredFact is one proposed emergent fact. Mirrors distillFact so prompt
// authors can keep the schema vocabulary consistent.
type DiscoveredFact struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Type       string      `json:"type"`
	Domain     flexStrings `json:"domain"`
	Confidence float64     `json:"confidence"`
	Entities   flexStrings `json:"entities"`
	Refs       flexStrings `json:"refs"`
	// Optional; absence means none. There is deliberately NO mechanical
	// stamping of the seed's motif (§2.1): wrong for a consequence whose
	// regularity is not the seed's, and futile for a keystone, whose subject IS
	// the mechanism and whose motif the ordinary subject strip therefore
	// removes. Keystones stay motif-less by design, with no strip exemption.
	Motifs flexStrings `json:"motifs"`
}

// parseDiscoverResponse is the symmetric companion of parseDistillResponse.
func parseDiscoverResponse(text string) (DiscoverResponse, error) {
	raw := extractJSON(text)
	var result DiscoverResponse
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return DiscoverResponse{}, fmt.Errorf("parseDiscoverResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}

// DiscoveryGates carries the verification gates from Plan 04. All three are
// applied at ingest time; the connected MCP agent is the only reasoner.
type DiscoveryGates struct {
	// ConfidenceThreshold is the minimum confidence a proposal must carry to
	// be written. ≥ comparison; threshold-ε is rejected.
	ConfidenceThreshold float64
	// DedupThreshold is the cosine-similarity floor above which a proposal
	// counts as a near-duplicate of an existing live fact and is rejected.
	// Source the value from EmbedderThresholds(emb).Dedup.
	DedupThreshold float64
	// BlastRadiusThreshold is the minimum required BlastRadius of the
	// proposal's seed-anchor (the bridge token's "support") for backward
	// keystones. Set to 0 to disable the gate (e.g. for forward direction).
	BlastRadiusThreshold int
}

// DiscoveryGatesFor resolves the verification gates for a discover step based on
// the direction. Forward (synthesis): confidence + dedup only. Backward
// (hypothesis): all three including BlastRadius. Thresholds come from the
// per-repo DiscoveryConfig accessors (Plan 03 Task 6); the dedup floor comes
// from the embedder's calibrated thresholds. Shared by the review (forward) and
// hypothesize (backward) MCP paths so the gate set is defined once.
func DiscoveryGatesFor(ri *repos.RepoInstance, dir DiscoverDirection) DiscoveryGates {
	g := DiscoveryGates{
		ConfidenceThreshold: ri.DiscoveryConfidenceThreshold(),
		DedupThreshold:      store.EmbedderThresholds(ri.Embedder()).Dedup,
	}
	if dir == DiscoverBackward {
		g.BlastRadiusThreshold = ri.DiscoveryBlastRadiusThreshold()
	}
	return g
}

// discoverResponseSchema describes the JSON shape the agent must return.
// Kept inline rather than a separate template — the prompt body already
// embeds the schema literal, and the dispatcher needs the same string for
// the MCP response_schema field.
const discoverResponseSchema = `{
  "type": "object",
  "properties": {
    "proposals": {
      "type": "array",
      "items": {
        "properties": {
          "path": {"type": "string"},
          "title": {"type": "string"},
          "body": {"type": "string"},
          "type": {"type": "string"},
          "domain": {"type": "array", "items": {"type": "string"}},
          "entities": {"type": "array", "items": {"type": "string"}},
          "motifs": {"type": "array", "items": {"type": "string"}, "description": "Motifs name the general regularity the claim instantiates, independent of its subject. The group's shared motif is a natural candidate if your proposal instantiates it. At most 3; zero is correct."},
          "confidence": {"type": "number"},
          "refs": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["path", "title", "body", "type", "confidence", "refs"]
      }
    },
    "reinforcements": {
      "type": "array",
      "items": {
        "properties": {
          "path": {"type": "string", "description": "An EXISTING corpus fact that already states what you would have proposed."},
          "reason": {"type": "string", "description": "One sentence on why the existing fact states the same claim. Required — if you cannot write it, they are not the same claim."},
          "refs": {"type": "array", "items": {"type": "string"}, "description": "Every seed fact above, joining that fact's refs as an independent derivation path."}
        },
        "required": ["path", "reason", "refs"]
      }
    }
  }
}`

// RenderDiscoverWorkItem builds the WorkItemContent (prompt + JSON schema)
// for a discover work item. The prompt is the strict default-skip variant
// from Plan 04 Task 1. Unlike the prune/distill/reflect renderers it cannot
// fail — the prompt is assembled from an in-memory payload with no marshal or
// template step — so it returns no error.
func RenderDiscoverWorkItem(payload DiscoverWorkPayload, ontologyRoot string) *WorkItemContent {
	return &WorkItemContent{
		Prompt:         renderDiscoverPrompt(payload, ontologyRoot),
		ResponseSchema: discoverResponseSchema,
	}
}

// renderDiscoverPrompt builds the agent-facing prompt for one discover work
// item. The strict default-skip language is the heart of Plan 04 Task 1:
// "Default to NO. Propose only if ALL conditions hold."
func renderDiscoverPrompt(payload DiscoverWorkPayload, ontologyRoot string) string {
	var b strings.Builder
	scopeLabel := payload.ScopeLabel
	if scopeLabel == "" {
		scopeLabel = "the scoped area"
	}
	if payload.Bridge.Token != "" {
		// Token-present variant (existing behaviour — unchanged).
		switch payload.Direction {
		case DiscoverBackward:
			b.WriteString("EMERGENT KEYSTONE DISCOVERY (BACKWARD)\n\n")
			b.WriteString("The facts below all share the structural token shown. They live in DIFFERENT cluster communities, so the shared token is a 'bridge' across regions of the knowledge base. We are looking for an UNSTATED PREMISE (a keystone hypothesis) that, if true, would explain why all of these facts are simultaneously true.\n\n")
		default:
			b.WriteString("EMERGENT CONSEQUENCE DISCOVERY (FORWARD)\n\n")
			b.WriteString("The facts below all share the structural token shown. They live in DIFFERENT cluster communities, so the shared token is a 'bridge' across regions of the knowledge base. We are looking for an UNSTATED CONSEQUENCE — a synthesis fact strictly entailed by the cited facts that nobody has written down yet.\n\n")
		}
		fmt.Fprintf(&b, "Bridge token: %q (kind=%s)\n", payload.Bridge.Token, payload.Bridge.Kind)
		// Far-lane SHIP line, blueprint §4 verbatim.
		//
		// It goes HERE — after the token line, before the members — because
		// its members have cohesion 0 by construction while the preamble just
		// above says they "share the structural token". A model reading only
		// that would reasonably infer they are also SIMILAR, which is the exact
		// inference the far lane exists to prevent.
		if payload.Lane == LaneFar {
			fmt.Fprintf(&b, "\nThese facts are NOT semantically similar. Each claims motif %q. Propose a keystone only if one mechanism genuinely underlies all members — default to NO.\n", payload.Bridge.Token)
		}
	} else {
		// Token-optional variant: scope-framed preamble.
		switch payload.Direction {
		case DiscoverBackward:
			b.WriteString("EMERGENT KEYSTONE DISCOVERY (BACKWARD)\n\n")
			fmt.Fprintf(&b, "The facts below are semantically related within %q but live in DIFFERENT sub-areas (sub-communities) of it — the link is latent. We are looking for an UNSTATED PREMISE (a keystone hypothesis) that, if true, would explain why all of these facts are simultaneously true.\n\n", scopeLabel)
		default:
			b.WriteString("EMERGENT CONSEQUENCE DISCOVERY (FORWARD)\n\n")
			fmt.Fprintf(&b, "The facts below are semantically related within %q but live in DIFFERENT sub-areas (sub-communities) of it — the link is latent. We are looking for an UNSTATED CONSEQUENCE — a synthesis fact strictly entailed by the cited facts that nobody has written down yet.\n\n", scopeLabel)
		}
	}
	fmt.Fprintf(&b, "Members (%d):\n", len(payload.Bridge.Members))
	for _, m := range payload.Bridge.Members {
		fmt.Fprintf(&b, "  - %s — %s\n", m.File, m.Title)
		if m.Body != "" {
			fmt.Fprintf(&b, "      %s\n", firstLine(m.Body))
		}
	}
	// GATE rider 2 (designer, 2026-08-23). The far-lane demo established that
	// condition (c) below is not answerable from this prompt at all: novelty
	// rests on the agent's default-skip rather than on the 0.92 dedup gate
	// (90d69628), and a cold agent with no corpus access would have proposed a
	// semantic duplicate that every shipped gate would then have passed.
	// Novelty is verified here, never assumed from context luck. It binds every
	// discover item — the hole is in the prompt, not in any one bridge kind.
	b.WriteString("\nBEFORE YOU ANSWER — QUERY THE CORPUS.\n")
	b.WriteString("Condition (c) below cannot be answered from this prompt. Query the corpus (knomit_query) for the claim you are considering, in the words you would use to state it, before you decide anything. The most likely failure of this task is re-deriving something the corpus already holds, and it will not look like a duplicate: an existing fact stating the same premise usually shares almost none of your wording.\n")
	b.WriteString("\nDECISION RULE — DEFAULT TO NO.\n")
	b.WriteString("Propose a fact ONLY IF ALL of these hold:\n")
	if payload.Direction == DiscoverBackward {
		b.WriteString("  (a) The proposed premise is strictly REQUIRED by the cited facts — if it were false, ≥2 of them would have to be revised.\n")
		b.WriteString("  (b) The premise is LOAD-BEARING — many corpus facts already depend on the bridge token.\n")
	} else {
		b.WriteString("  (a) The proposed consequence is strictly ENTAILED by the cited facts — it follows necessarily from their conjunction, not as a plausible extension.\n")
		b.WriteString("  (b) The consequence is LOAD-BEARING — its falsity invalidates ≥2 of the cited facts.\n")
	}
	b.WriteString("  (c) Not already in the corpus — you QUERIED for it above and no existing fact states it. If one does, REINFORCE it instead of proposing (below).\n")
	b.WriteString("  (d) You can cite every seed fact above in refs. An empty refs array indicates you did not engage with the inputs.\n\n")
	b.WriteString("If any condition fails, return an empty proposals array. Skipping is the expected outcome.\n\n")
	// GATE rider 3 (designer, 2026-08-23): discovery's third outcome. On a
	// recall hit the answer is neither propose nor decline — the seeds are an
	// INDEPENDENT derivation of a claim the corpus already holds, and recording
	// that is corroboration. The equivalence claim gets alias-judge discipline
	// for the same reason the alias judge does: an equivalence nobody could
	// justify in a sentence is the hallucinated one, and over-merge is
	// invisible downstream.
	b.WriteString("REINFORCE — the third outcome, for when the corpus already states it.\n")
	b.WriteString("If your query found a fact that already states what you would have proposed, do not propose it again and do not simply fall silent: REINFORCE that fact. Reinforcement records these seeds as an INDEPENDENT derivation of a claim the corpus already holds — one more proof that the wheel is round — by adding them to its refs, incrementing its sources and strengthening its evidence. Nothing else about the fact changes.\n")
	b.WriteString("Reinforce ONLY IF ALL of these hold:\n")
	b.WriteString("  (a) The existing fact states the SAME claim, not a neighbouring one. Say why in one sentence; if you cannot write that sentence, they are not the same claim.\n")
	b.WriteString("  (b) You would otherwise have proposed it here. Reinforcement replaces a proposal; it is never a note about something you happened to read.\n")
	b.WriteString("  (c) You can cite every seed fact above in refs.\n")
	b.WriteString("DEFAULT TO NO on sameness. When you are torn between \"the same claim\" and \"a claim near it\", PROPOSE AND LINK instead of reinforcing: a false link is recoverable, a false merge is not.\n\n")
	b.WriteString("PERSISTENCE — origin reflects how this group was formed.\n")
	b.WriteString("These facts were grouped by a cross-cluster BRIDGE, so any fact you persist from them is discovery-engine output (origin: discovered). Submit your proposals back via knomit_hypothesize/knomit_review to record them automatically. If you instead save one directly with knomit_learn after previewing, you MUST set origin: discovered and cite every seed fact above in refs.\n\n")
	b.WriteString("RESPONSE SCHEMA: {\"proposals\":[{\"path\":\"" + ontologyRoot + "/.../slug.md\",\"title\":\"...\",\"body\":\"...\",\"type\":\"")
	if payload.Direction == DiscoverBackward {
		b.WriteString("hypothesis")
	} else {
		b.WriteString("synthesis")
	}
	b.WriteString("\",\"domain\":[],\"entities\":[],\"confidence\":0.0,\"refs\":[]}],\"reinforcements\":[{\"path\":\"" + ontologyRoot + "/.../existing.md\",\"reason\":\"...\",\"refs\":[]}]}\n")
	return b.String()
}

// applyDiscoveredProposals runs every proposal through the strict gate chain
// and writes the survivors as facts with origin=discovered.
//
// Gate order (cheapest-first, then load-bearing):
//
//  1. type-vs-direction sanity (forward→synthesis, backward→hypothesis)
//  2. validateOutputPath / validateOutputType
//  3. refs MUST cite every bridge seed (proves the agent engaged)
//  4. confidence ≥ ConfidenceThreshold
//  5. embedding dedup vs the live corpus (skip if emb is nil)
//  6. backward only: BlastRadius(seed anchor) ≥ BlastRadiusThreshold
//
// Each rejection is logged at debug and emitted as a "warn" ProgressEvent so
// the surrounding session log surfaces it; nothing is fatal — bad proposals
// are dropped, good ones land.
func applyDiscoveredProposals(
	ctx context.Context,
	gs store.FactIndex,
	idx SearchQuery,
	emb store.Embedder,
	payload DiscoverWorkPayload,
	proposals []DiscoveredFact,
	gates DiscoveryGates,
	branch string,
	// localRepoID is this repo's 12-hex id. Refs are stored canonical
	// (kb://<own-id>/<path>), so the lineage filter below needs it to tell a
	// local edge from a foreign one; passing "" reads them all as foreign.
	localRepoID string,
	ontologyRoot string,
	onProgress func(ProgressEvent),
) ([]string, error) {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	wantType := fact.Synthesis
	if payload.Direction == DiscoverBackward {
		wantType = fact.Hypothesis
	}

	// Pre-compute the union of seed paths for the refs-completeness check.
	seedPaths := make(map[string]struct{}, len(payload.Bridge.Members))
	for _, m := range payload.Bridge.Members {
		seedPaths[m.File] = struct{}{}
	}

	gate := refs.New(localRepoID, refs.FromFactQuery(idx, branch))

	var written []string
	for _, p := range proposals {
		if fact.Type(p.Type) != wantType {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: type=%s does not match direction=%s", p.Type, payload.Direction)})
			continue
		}
		if err := validateOutputPath(p.Path, ontologyRoot); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: %v", err)})
			continue
		}
		if err := validateOutputType(p.Type); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery %s rejected: %v", p.Path, err)})
			continue
		}
		if !refsCoverSeeds(p.Refs, seedPaths) {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: %s refs does not cite every seed", p.Path)})
			continue
		}
		if p.Confidence < gates.ConfidenceThreshold {
			log.Debug().Str("path", p.Path).Float64("confidence", p.Confidence).Float64("threshold", gates.ConfidenceThreshold).Msg("discovery gate: confidence below threshold")
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: %s confidence %.2f < threshold %.2f", p.Path, p.Confidence, gates.ConfidenceThreshold)})
			continue
		}

		path := normalizeFactPath(p.Path)
		localRefs := localFactRefPaths(p.Refs, localRepoID)
		weight := computeWeight(ctx, gs, branch, localRepoID, localRefs)

		f := fact.NewFact(path)
		f.Title = p.Title
		f.Body = p.Body
		f.Type = wantType
		f.Domain = p.Domain
		f.Confidence = p.Confidence
		// SHARE, so 1 — and more emphatically than distill: a bridge is
		// SELECTED for facts that already co-occur across derivations, so
		// shared ancestry is the expected case here, not the corner case.
		// Pooling would double-count it by construction.
		f.Sources = 1
		f.Entities = p.Entities
		// Drop rather than lose the proposal — see ApplyPruneDecisions. A
		// discovered fact costs a bridge enumeration and an LLM call to
		// produce; discarding it over a malformed motif spends both for nothing.
		f.Motifs = fact.DropInvalidMotifs(p.Motifs)
		f.EvidenceWeight = weight
		f.Origin = fact.Discovered

		// Same gate as every other write path. Discovery retracts nothing — a
		// bridge proposal is a NEW claim over facts that stay alive — so there
		// is no retraction list. A rejection warns and skips this one proposal,
		// matching every other gate in this loop.
		canonRefs, _, gerr := gate.Apply(ctx, f.Path(), p.Refs, nil)
		if gerr != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery %s rejected: %v", f.Path(), gerr)})
			continue
		}
		f.Refs = canonRefs

		dup, err := isDuplicate(ctx, idx, emb, branch, f, gates.DedupThreshold)
		if err != nil {
			// Embedder error → cannot determine duplicity; treat as non-duplicate so
			// the remaining gates (BlastRadius etc.) decide. Dropping would silently
			// discard valid proposals whenever the embedder is unavailable.
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery dedup check failed for %s: %v; treating as non-duplicate", f.Path(), err)})
		} else if dup {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: %s is a near-duplicate of an existing live fact", f.Path())})
			continue
		}

		if payload.Direction == DiscoverBackward && gates.BlastRadiusThreshold > 0 {
			keep := false
			for member := range seedPaths {
				radius, brErr := idx.BlastRadius(ctx, branch, member)
				if brErr != nil {
					log.Debug().Err(brErr).Str("member", member).Msg("discovery: BlastRadius lookup failed")
					continue
				}
				if radius >= gates.BlastRadiusThreshold {
					keep = true
					break
				}
			}
			if !keep {
				onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery rejected: %s seed-anchor BlastRadius below threshold %d", f.Path(), gates.BlastRadiusThreshold)})
				continue
			}
		}

		content, err := fact.SerializeFact(f)
		if err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery serialize %s: %v", f.Path(), err)})
			continue
		}
		// Commit message: token-anchored when Token is set; fall back to the
		// scope label (or "scoped") for token-optional filtered bridges so the
		// message never renders the literal empty string `""`.
		bridgeRef := payload.Bridge.Token
		if bridgeRef == "" {
			bridgeRef = payload.ScopeLabel
			if bridgeRef == "" {
				bridgeRef = "scoped"
			}
		}
		msg := fmt.Sprintf("discover-%s: emergent fact via bridge %q", payload.Direction, bridgeRef)
		if _, err := gs.WriteFact(ctx, branch, f.Path(), content, msg, "discover"); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("discovery write %s: %v", f.Path(), err)})
			continue
		}
		onProgress(ProgressEvent{Phase: "detail-discover", Message: "discover " + f.Path()})
		written = append(written, f.Path())
	}
	return written, nil
}

// refsCoverSeeds reports whether refs is a superset of the seed paths set.
func refsCoverSeeds(refs []string, seedPaths map[string]struct{}) bool {
	have := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		have[r] = struct{}{}
	}
	for s := range seedPaths {
		if _, ok := have[s]; !ok {
			return false
		}
	}
	return true
}

// isDuplicate computes the document embedding for the proposal and reports
// whether the live corpus already contains a fact within DedupThreshold cosine
// similarity. When emb is nil (embeddings disabled), the gate is a no-op.
func isDuplicate(ctx context.Context, idx SearchQuery, emb store.Embedder, branch string, f fact.Fact, threshold float64) (bool, error) {
	if emb == nil {
		return false, nil
	}
	vec, err := emb.EmbedDocument(ctx, f.Title, f.Body)
	if err != nil {
		return false, fmt.Errorf("isDuplicate: embed: %w", err)
	}
	res, err := idx.Search(ctx, branch, store.SearchOptions{
		QueryVec:      vec,
		MinSimilarity: threshold,
		Limit:         1,
	})
	if err != nil {
		return false, fmt.Errorf("isDuplicate: search: %w", err)
	}
	return len(res) > 0, nil
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}
