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
	b.WriteString("  (d) You can cite AT LEAST TWO of the seed facts above in refs, as the derivation path this claim actually rests on. Cite the seeds that genuinely support it and NO OTHERS — a seed that does not fit is simply not claimed as evidence, not forced in, and not a reason to abandon an otherwise valid derivation. Refs naming anything that is not a seed above are discarded. An empty refs array indicates you did not engage with the inputs.\n\n")
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
	b.WriteString("  (c) You can cite AT LEAST TWO of the seed facts above in refs, as the derivation path — the seeds that genuinely support the existing claim, and no others.\n")
	b.WriteString("DEFAULT TO NO on sameness. When you are torn between \"the same claim\" and \"a claim near it\", PROPOSE AND LINK instead of reinforcing: a false link is recoverable, a false merge is not.\n\n")
	b.WriteString("PERSISTENCE — origin reflects how this group was formed.\n")
	b.WriteString("These facts were grouped by a cross-cluster BRIDGE, so any fact you persist from them is discovery-engine output (origin: discovered). Submit your proposals back via knomit_hypothesize/knomit_review to record them automatically. If you instead save one directly with knomit_learn after previewing, you MUST set origin: discovered and cite at least two of the seed facts above in refs — the ones on the derivation path, and no facts other than the seeds.\n\n")
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
		// ONE expression of the floor, both call sites. The proposal and
		// reinforce paths ask the same question and must not drift into two
		// spellings of it — the same reason the motif rank order has a single
		// comparator. splitSeedRefs is called again here only for the message
		// and the discard list; the DECISION is refsCiteSeedSubset's alone.
		if !refsCiteSeedSubset(p.Refs, seedPaths, localRepoID) {
			_, _, distinctSeeds := splitSeedRefs(p.Refs, seedPaths, localRepoID)
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf(
				"discovery rejected: %s cites %d of the bridge's seeds as its derivation path, needs at least %d",
				p.Path, distinctSeeds, minCitedSeeds)})
			continue
		}
		citedSeeds, extraRefs, _ := splitSeedRefs(p.Refs, seedPaths, localRepoID)
		// SEEDS ONLY, discarded not rejected — the same treatment the reinforce
		// path has always given surplus refs, so the two paths now agree.
		//
		// A derived fact's refs ARE its derivation path: every one becomes a
		// permanent DERIVED_FROM edge and moves the fact's evidence weight, so a
		// ref naming something the bridge never offered is a derivation nobody
		// checked. Before #151 this path wrote them through unfiltered.
		//
		// Discarding rather than rejecting keeps the campaign's rule that a
		// proposal costing a bridge enumeration and an LLM call is not thrown
		// away over surplus (cf. DropInvalidMotifs) — and the warning is what
		// stops the drop being silent. NOTE the consequence: this drops
		// external refs too (an https:// source the model attached), because
		// the discover prompt asks for a derivation path over the seeds, not a
		// bibliography.
		if len(extraRefs) > 0 {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf(
				"discovery %s: discarded %d ref(s) naming facts outside the bridge: %s",
				p.Path, len(extraRefs), strings.Join(extraRefs, ", "))})
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
		canonRefs, _, gerr := gate.Apply(ctx, f.Path(), citedSeeds, nil)
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

// minCitedSeeds is the floor on how many of a bridge's offered seeds a derived
// fact must claim as its derivation path.
//
// TWO, because one is not a derivation. A bridge asserts that a claim follows
// from the CONJUNCTION of facts that clustering kept apart; a fact citing a
// single seed is not a weaker bridge, it is a different thing entirely — an
// observation about one fact, which the bridge machinery is not the way to
// record. This is a floor on ARGUMENT SHAPE, not a corpus property: it is two
// because a conjunction needs two terms, and no measurement of any corpus
// could move it (MN13).
//
// CONSEQUENCE WORTH STATING, because it is the first thing a reader will get
// wrong: on a TWO-seed bridge this rule is identical to the all-seeds rule it
// replaces. #151 relaxes nothing there, by design. The relaxation bites only
// at three or more seeds — which is where it was measured to bite (s207's
// discarded derivation was a five-seed item).
const minCitedSeeds = 2

// splitSeedRefs partitions a derived fact's refs into the OFFERED SEEDS it
// claims and the EXTRAS it does not, comparing on canonical fact paths rather
// than raw strings.
//
// THE NORMALISATION IS LOAD-BEARING, and the raw-string version it replaces was
// a latent defect. Refs are STORED CANONICAL as kb://<own-id12>/<path>, so a
// seed cited in the form the corpus itself writes matched NOTHING under a
// literal string compare. Under the all-seeds rule that failed loudly (a valid
// answer was rejected); under the subset rule it fails twice over — the
// correctly-spelled seed does not COUNT toward minCitedSeeds and is then
// discarded as an extra, so relaxing the gate without this would have thrown
// away the very answers #151 exists to keep. Third live site of the same
// stored-canonical trap (#125's lineage refs, #132's self-refs, this).
//
// Returns the cited seeds in their ORIGINAL spelling (callers canonicalise
// deliberately, and the reinforce path must not rewrite authored strings), the
// extras likewise, and the count of DISTINCT seeds cited — distinct, so a fact
// naming one seed twice, in two spellings, does not buy itself a second term.
func splitSeedRefs(refs []string, seedPaths map[string]struct{}, localRepoID string) (cited, extras []string, distinct int) {
	canonSeed := make(map[string]struct{}, len(seedPaths))
	for s := range seedPaths {
		canonSeed[fact.ClassifyRef(s, localRepoID).Path] = struct{}{}
	}
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		c := fact.ClassifyRef(r, localRepoID)
		if _, isSeed := canonSeed[c.Path]; isSeed && c.Kind == fact.RefLocalFact {
			cited = append(cited, r)
			if _, dup := seen[c.Path]; !dup {
				seen[c.Path] = struct{}{}
				distinct++
			}
			continue
		}
		extras = append(extras, r)
	}
	return cited, extras, distinct
}

// refsCiteSeedSubset reports whether a derived fact's refs form an acceptable
// derivation path over the bridge's offered seeds: a SUBSET of them, at least
// minCitedSeeds distinct.
//
// This replaces the "cite EVERY offered seed" rule (designer ruling
// 2026-08-26, #151). That rule PUNISHED JUDGE HONESTY: when four of five seeds
// genuinely derived the target and the fifth did not, there was no subset
// mechanism, so a valid independent derivation was discarded whole rather than
// recorded at its true strength. It also FORCED the exact over-citation that
// #125 (lineage) and #132 (self-reference) forbid, so the three rules were
// fighting each other; this aligns them.
//
// WHAT IT DOES NOT LICENSE: inventing a citation. Only offered seeds count
// toward the floor, and refs naming anything else are the caller's to discard —
// a derived fact's refs are its derivation path, and a path through a fact the
// bridge never offered is a claim nobody checked.
func refsCiteSeedSubset(refs []string, seedPaths map[string]struct{}, localRepoID string) bool {
	_, _, distinct := splitSeedRefs(refs, seedPaths, localRepoID)
	return distinct >= minCitedSeeds
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
