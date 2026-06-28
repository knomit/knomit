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
	Direction DiscoverDirection `json:"direction"`
	Bridge    BridgeSeedSet     `json:"bridge"`
}

// DiscoverResponse is the LLM JSON contract for one discover work item. The
// agent returns zero or more proposed facts. Empty is the expected outcome —
// the strict default-skip instructions are reinforced by the prompt (Plan 04
// Task 1). Surviving proposals are then run through confidence/dedup/blast
// gates before any write.
type DiscoverResponse struct {
	Proposals []DiscoveredFact `json:"proposals"`
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
}

// ParseDiscoverResponse is the public form used by callers outside the
// synthesize package (e.g. the MCP hypothesize handler).
func ParseDiscoverResponse(text string) (DiscoverResponse, error) {
	return parseDiscoverResponse(text)
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
          "confidence": {"type": "number"},
          "refs": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["path", "title", "body", "type", "confidence", "refs"]
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
	switch payload.Direction {
	case DiscoverBackward:
		b.WriteString("EMERGENT KEYSTONE DISCOVERY (BACKWARD)\n\n")
		b.WriteString("The facts below all share the structural token shown. They live in DIFFERENT cluster communities, so the shared token is a 'bridge' across regions of the knowledge base. We are looking for an UNSTATED PREMISE (a keystone hypothesis) that, if true, would explain why all of these facts are simultaneously true.\n\n")
	default:
		b.WriteString("EMERGENT CONSEQUENCE DISCOVERY (FORWARD)\n\n")
		b.WriteString("The facts below all share the structural token shown. They live in DIFFERENT cluster communities, so the shared token is a 'bridge' across regions of the knowledge base. We are looking for an UNSTATED CONSEQUENCE — a synthesis fact strictly entailed by the cited facts that nobody has written down yet.\n\n")
	}
	fmt.Fprintf(&b, "Bridge token: %q (kind=%s)\n", payload.Bridge.Token, payload.Bridge.Kind)
	fmt.Fprintf(&b, "Members (%d):\n", len(payload.Bridge.Members))
	for _, m := range payload.Bridge.Members {
		fmt.Fprintf(&b, "  - %s — %s\n", m.File, m.Title)
		if m.Body != "" {
			fmt.Fprintf(&b, "      %s\n", firstLine(m.Body))
		}
	}
	b.WriteString("\nDECISION RULE — DEFAULT TO NO.\n")
	b.WriteString("Propose a fact ONLY IF ALL of these hold:\n")
	if payload.Direction == DiscoverBackward {
		b.WriteString("  (a) The proposed premise is strictly REQUIRED by the cited facts — if it were false, ≥2 of them would have to be revised.\n")
		b.WriteString("  (b) The premise is LOAD-BEARING — many corpus facts already depend on the bridge token.\n")
	} else {
		b.WriteString("  (a) The proposed consequence is strictly ENTAILED by the cited facts — it follows necessarily from their conjunction, not as a plausible extension.\n")
		b.WriteString("  (b) The consequence is LOAD-BEARING — its falsity invalidates ≥2 of the cited facts.\n")
	}
	b.WriteString("  (c) Not already in the corpus — no existing fact already states it.\n")
	b.WriteString("  (d) You can cite every seed fact above in refs. An empty refs array indicates you did not engage with the inputs.\n\n")
	b.WriteString("If any condition fails, return an empty proposals array. Skipping is the expected outcome.\n\n")
	b.WriteString("RESPONSE SCHEMA: {\"proposals\":[{\"path\":\"" + ontologyRoot + "/.../slug.md\",\"title\":\"...\",\"body\":\"...\",\"type\":\"")
	if payload.Direction == DiscoverBackward {
		b.WriteString("hypothesis")
	} else {
		b.WriteString("synthesis")
	}
	b.WriteString("\",\"domain\":[],\"entities\":[],\"confidence\":0.0,\"refs\":[]}]}\n")
	return b.String()
}

// ApplyDiscoveredProposals is the public entry to applyDiscoveredProposals
// for the MCP hypothesize handler.
func ApplyDiscoveredProposals(
	ctx context.Context,
	gs store.FactIndex,
	idx store.SearchIndex,
	emb store.Embedder,
	payload DiscoverWorkPayload,
	proposals []DiscoveredFact,
	gates DiscoveryGates,
	branch string,
	ontologyRoot string,
	onProgress func(ProgressEvent),
) ([]string, error) {
	return applyDiscoveredProposals(ctx, gs, idx, emb, payload, proposals, gates, branch, ontologyRoot, onProgress)
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
	idx store.SearchIndex,
	emb store.Embedder,
	payload DiscoverWorkPayload,
	proposals []DiscoveredFact,
	gates DiscoveryGates,
	branch string,
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
		var localRefs []string
		for _, r := range p.Refs {
			if strings.HasSuffix(r, ".md") {
				localRefs = append(localRefs, r)
			}
		}
		weight := computeWeight(ctx, gs, branch, localRefs)

		f := fact.NewFact(path)
		f.Title = p.Title
		f.Body = p.Body
		f.Type = wantType
		f.Domain = p.Domain
		f.Confidence = p.Confidence
		f.Sources = 1
		f.Entities = p.Entities
		f.Refs = p.Refs
		f.EvidenceWeight = weight
		f.Origin = fact.Discovered

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
		msg := fmt.Sprintf("discover-%s: emergent fact via bridge %q", payload.Direction, payload.Bridge.Token)
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
func isDuplicate(ctx context.Context, idx store.SearchIndex, emb store.Embedder, branch string, f fact.Fact, threshold float64) (bool, error) {
	if emb == nil {
		return false, nil
	}
	vec, err := emb.EmbedDocument(f.Title, f.Body)
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
