package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"
)

// localEvidenceRefs returns the subset of a fact's refs that are genuinely-local
// fact edges eligible to contribute evidence weight. It drops:
//   - the fact's own resulting path (a dedup-merge appends it as lineage; a fact
//     is never its own evidence source), and
//   - kb:// cross-repo refs, which point into another repo and are External, not
//     local fact edges — mirroring classifyRefs (explain.go) so this filter and
//     the provenance graph agree on what "local" means, even for a ref ending in
//     .md. ComputeEvidenceWeight only weighs genuinely local refs.
func localEvidenceRefs(f fact.Fact) []string {
	var localRefs []string
	for _, r := range f.Refs {
		if r == f.Path() {
			continue
		}
		if !strings.HasPrefix(r, federate.KBScheme) && strings.HasSuffix(r, ".md") {
			localRefs = append(localRefs, r)
		}
	}
	return localRefs
}

// learnTool returns the Tool definition for knomit_learn.
func learnTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_learn",
		mcpgo.WithDescription("Write one or more facts to the knowledge base in a single commit."),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this learning moment."),
		),
		mcpgo.WithArray("facts",
			mcpgo.Required(),
			mcpgo.Description("Array of fact objects to write."),
			mcpgo.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topic":      map[string]any{"type": "string", "description": "Top-level ontology topic (e.g. technology, people, science)."},
					"category":   map[string]any{"type": "string", "description": "Category path within the topic (e.g. languages/go/concurrency)."},
					"title":      map[string]any{"type": "string", "description": "Fact title (short, descriptive)."},
					"body":       map[string]any{"type": "string", "description": "Fact body in natural language. State compound conditions in full — name every component; never abbreviate a multi-part condition into a catchier summary. Include the consequence a consumer should act on. For rules and policies, name the foreseeable misreading (what the fact does NOT mean) if you can see one."},
					"kind":       map[string]any{"type": "string", "description": "Classification family. epistemic (default) for descriptive knowledge — what is. pragmatic for prescriptive knowledge — what to do. The allowed `type` values depend on the kind.", "default": "epistemic", "enum": []string{"epistemic", "pragmatic"}},
					"type":       map[string]any{"type": "string", "description": "Leaf type. When kind=epistemic: observation (default, concrete facts), concept (definitions), process (procedures), principle (rules), pattern (recurring structures), reference (specs/measurements), synthesis (derived from other facts), insight (a non-obvious grounded conclusion drawn from connecting facts you already trust), hypothesis (predictions from patterns — carries uncertainty), methodology (reasoning process lessons). When kind=pragmatic: policy (mandatory rules), heuristic (rules-of-thumb).", "default": "observation"},
					"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Cross-cutting domain tags."},
					"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0.", "default": 0.7},
					"sources":    map[string]any{"type": "integer", "description": "Number of independent sources.", "default": 1},
					"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Entities this fact mentions."},
					"refs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "External URLs or source references."},
					"origin":     map[string]any{"type": "string", "description": "Which pipeline minted this fact — NOT where the information came from. Omit for any fact you write yourself; it defaults to authored, and that includes facts transcribed from an external source you read (discovered does NOT mean 'I learned this from a source'). distilled = synthesis-pipeline output from a regular cluster (type synthesis only); discovered = discovery-engine output from a cross-cluster bridge (type synthesis or hypothesis only) — any other type with these origins is rejected. When persisting a previewed discover/distill proposal, set the origin that work-item's prompt specifies — origin records how the candidate group was formed, not whether it was reviewed. origin is immutable: knomit_update cannot change it; fixing a wrong origin requires knomit_retract plus a fresh knomit_learn.", "enum": []string{"authored", "distilled", "discovered"}},
				},
				"required": []string{"topic", "category", "title", "body"},
			}),
		),
	)
}

// learnFactInput is the JSON shape of a single fact in the input array.
type learnFactInput struct {
	Topic      string   `json:"topic"`
	Category   string   `json:"category"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Kind       string   `json:"kind"`
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
	Origin     string   `json:"origin"`
}

// LearnHandler returns the handler function for knomit_learn.
// If embedder is non-nil, dedup checks batch-embed all incoming facts upfront
// instead of embedding one-at-a-time inside each Search call.
func LearnHandler(embedders ...store.BatchEmbedder) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	var batchEmb store.BatchEmbedder
	if len(embedders) > 0 {
		batchEmb = embedders[0]
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
		}
		ri := b.Write()
		s, release, err := storeIndices(ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer release()
		agentBranch := ri.AgentBranch()
		ontologyRoot := ri.OntologyRoot()
		ontology := ri.Ontology()

		// 1. Parse arguments.
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// Parse facts from the arguments.
		var factInputs []learnFactInput
		if err := unmarshalArg(req, "facts", &factInputs); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if len(factInputs) == 0 {
			return mcpgo.NewToolResultError("facts must not be empty"), nil
		}

		// Validate batch type consistency: cannot mix observed and inferred types.
		hasObserved, hasInferred := false, false
		for _, fi := range factInputs {
			eType := fact.Type(fi.Type)
			if eType == "" {
				eType = fact.DefaultEpistemicType
			}
			if eType == fact.Hypothesis || eType == fact.Methodology {
				hasInferred = true
			} else {
				hasObserved = true
			}
		}
		if hasObserved && hasInferred {
			return mcpgo.NewToolResultError("cannot mix observed types (observation, concept, etc.) and inferred types (hypothesis, methodology) in a single learn call"), nil
		}

		// 3. Validate inputs, build paths, and serialize facts.
		files := make(map[string]string, len(factInputs))
		facts := make([]fact.Fact, len(factInputs))
		// topicCategories[i] is the ontology path for facts[i]; cached here so
		// the dedup-merge branch below can re-run ValidateFact without
		// re-deriving (or losing access to) the topic path.
		topicCategories := make([]string, len(factInputs))
		for i, fi := range factInputs {
			// Validate topic+category against ontology.
			topicCategory := fi.Topic
			if fi.Category != "" {
				topicCategory = fi.Topic + "/" + fi.Category
			}
			topicCategories[i] = topicCategory
			if ontology != nil {
				if err := ontology.ValidatePath(topicCategory); err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: %v", i, err)), nil
				}
			}
			// Validate category.
			if strings.TrimSpace(fi.Category) == "" {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: category is required", i)), nil
			}
			// Build path with server-generated UUID.
			path := fact.BuildFactPath(ontologyRoot, fi.Topic, fi.Category)

			domain := fi.Domain
			if domain == nil {
				domain = []string{}
			}
			entities := fi.Entities
			if entities == nil {
				entities = []string{}
			}
			refs := fi.Refs
			if refs == nil {
				refs = []string{}
			}
			// Resolve kind and leaf type. SerializeFact (called below)
			// validates the (kind, type) pair via the same path that
			// ParseFact uses, so we don't pre-validate here.
			kind := fact.Kind(fi.Kind)
			if kind == "" {
				kind = fact.DefaultKind
			}
			eType := fact.Type(fi.Type)
			if eType == "" && kind == fact.Epistemic {
				eType = fact.DefaultEpistemicType
			}
			f := fact.NewFact(path)
			f.Title = fi.Title
			f.Body = fi.Body
			f.Kind = kind
			f.Type = eType
			f.Domain = domain
			f.Confidence = fi.Confidence
			f.Sources = fi.Sources
			f.Entities = entities
			f.Refs = refs
			// Explicit origin: set it and let SerializeFact validate, the
			// same way (kind, type) is handled above — one chokepoint, so
			// synthesize and web write paths get the identical check.
			// Empty is left unset so SerializeFact/ParseFact apply their
			// defaults (authored, or distilled for legacy synthesis facts).
			// This is how an agent persists a previewed discovery proposal
			// as origin=discovered.
			if fi.Origin != "" {
				f.Origin = fact.Origin(fi.Origin)
			}
			if ontology != nil {
				if err := fact.ValidateFact(ontology, topicCategory, f); err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: %v", i, err)), nil
				}
			}
			facts[i] = f
			serialized, err := fact.SerializeFact(f)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: serialize: %v", i, err)), nil
			}
			files[path] = serialized
		}

		// 3b. Dedup check: search for near-duplicates scoped to the same category directory.
		// Batch-embed all incoming facts upfront if a BatchEmbedder is available,
		// so each dedup Search uses the pre-computed vector instead of re-embedding.
		// The near-duplicate cosine floor is model-dependent (see internal/retrieval).
		dedupThreshold := store.EmbedderThresholds(batchEmb).Dedup
		var dedupVecs [][]float32
		if batchEmb != nil && len(facts) > 0 {
			titles := make([]string, len(facts))
			bodies := make([]string, len(facts))
			for i, f := range facts {
				titles[i] = f.Title
				bodies[i] = f.Body
			}
			var embErr error
			dedupVecs, embErr = batchEmb.EmbedDocuments(ctx, titles, bodies)
			if embErr != nil {
				log.Warn().Err(embErr).Int("count", len(titles)).Msg("learn: batch embed failed; dedup falls back to per-fact embedding and donations are skipped")
				dedupVecs = nil
			}
		}
		// donatePaths[i] is the on-disk path that dedupVecs[i] corresponds to,
		// or "" to suppress donation (used when the dedup-merge branch decided
		// to keep the existing fact's title+body, so our vector — computed
		// over f.Title+f.Body — would not match what gets written).
		donatePaths := make([]string, len(facts))
		for i, f := range facts {
			donatePaths[i] = f.Path()
		}
		// Hypotheses subsumed by an incoming observation, retracted in the same
		// commit as the write that subsumes them (see the subsume branch below).
		var retract []string
		for i, f := range facts {
			categoryDir := f.Path()[:strings.LastIndex(f.Path(), "/")]
			sq := store.SearchOptions{
				Text:          f.Title + " " + f.Body,
				Path:          categoryDir,
				MinSimilarity: dedupThreshold,
				Limit:         1,
			}
			if dedupVecs != nil && i < len(dedupVecs) && len(dedupVecs[i]) > 0 {
				sq.QueryVec = dedupVecs[i]
			}
			results, err := s.search.Search(ctx, agentBranch, sq)
			if err != nil || len(results) == 0 {
				continue
			}

			match := results[0]
			// Read existing fact to get its full metadata (refs, etc.)
			readResult, readErr := s.facts.ReadFact(ctx, agentBranch, match.Path, nil)
			if readErr != nil {
				continue
			}
			existingFact, parseErr := fact.ParseFact(match.Path, readResult.Content)
			if parseErr != nil {
				continue
			}

			// Type-aware dedup: if existing fact is a hypothesis and new fact is not,
			// the observation subsumes the hypothesis.
			if existingFact.Type == fact.Hypothesis && f.Type != fact.Hypothesis {
				// Write the observation as normal (don't merge into existing path).
				// The hypothesis is retracted in the SAME commit as the write that
				// subsumes it (staged here, applied by BatchWriteFacts below). A
				// separate DeleteFact would be a second commit: if it failed we
				// would still write an observation whose refs point at a live
				// hypothesis it claims to have subsumed, and if the batch write
				// then failed we would have retracted a fact for nothing.
				// AppendUnique: two incoming observations can match the same
				// hypothesis, and deleting one path twice in a batch is at best
				// wasted tree work.
				retract = fact.AppendUnique(retract, match.Path)
				// Add hypothesis path to observation's refs.
				f.Refs = fact.AppendUnique(f.Refs, match.Path)
				facts[i] = f
				serialized, err := fact.SerializeFact(f)
				if err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: serialize subsumed: %v", i, err)), nil
				}
				files[f.Path()] = serialized
				continue
			}

			// Determine winner by merge rule: higher confidence wins, tie-break by sources.
			newConf := f.Confidence
			existConf := existingFact.Confidence

			var merged fact.Fact
			if newConf > existConf || (newConf == existConf && f.Sources >= existingFact.Sources) {
				// New fact wins — keep new fact's title and body, write to existing path.
				merged = fact.NewFact(match.Path)
				merged.Title = f.Title
				merged.Body = f.Body
				merged.Kind = f.Kind
				merged.Type = f.Type
				merged.Domain = fact.UnionStrings(f.Domain, existingFact.Domain)
				merged.Entities = fact.UnionStrings(f.Entities, existingFact.Entities)
				merged.Confidence = max(newConf, existConf)
				merged.Sources = f.Sources + existingFact.Sources
				merged.Refs = fact.AppendUnique(fact.UnionStrings(f.Refs, existingFact.Refs), match.Path)
				// New fact's identity wins, so its origin wins too.
				merged.Origin = f.Origin
				// dedup vector still describes the merged content (same title+body
				// as the new fact); just retarget to the existing path it now lives at.
				donatePaths[i] = merged.Path()
			} else {
				// Existing fact wins — keep existing title and body, update metadata.
				merged = fact.NewFact(match.Path)
				merged.Title = existingFact.Title
				merged.Body = existingFact.Body
				merged.Kind = existingFact.Kind
				merged.Type = existingFact.Type
				merged.Domain = fact.UnionStrings(f.Domain, existingFact.Domain)
				merged.Entities = fact.UnionStrings(f.Entities, existingFact.Entities)
				merged.Confidence = max(newConf, existConf)
				merged.Sources = f.Sources + existingFact.Sources
				merged.Refs = fact.AppendUnique(fact.UnionStrings(f.Refs, existingFact.Refs), match.Path)
				// Existing fact's identity wins, so its origin wins too.
				merged.Origin = existingFact.Origin
				// dedup vector was computed over f's title+body; existing wins so
				// merged content differs. Drop the donation — upsert will fall
				// through to a fresh embed.
				donatePaths[i] = ""
			}

			// Re-validate the merged fact against ontology rules. The merge
			// may union entities/domain or pull values from the existing
			// fact, so the result is not guaranteed to satisfy rules just
			// because the input did. Without this re-check, a violation
			// would surface later as an opaque serialize error.
			if ontology != nil {
				if err := fact.ValidateFact(ontology, topicCategories[i], merged); err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: dedup-merge: %v", i, err)), nil
				}
			}

			// Remove the original new-fact path from the files map and add the merged one.
			delete(files, f.Path())
			serialized, err := fact.SerializeFact(merged)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: serialize merged: %v", i, err)), nil
			}
			files[merged.Path()] = serialized
			facts[i] = merged
		}

		// Compute evidence_weight for machine-origin derived facts (distilled/
		// discovered) that cite local facts, mirroring the synthesis/discovery
		// pipeline. Scoped to non-authored origins so ordinary learn calls are
		// unaffected — only previewed-then-saved pipeline output gets a weight.
		for i := range facts {
			f := facts[i]
			if f.Origin != fact.Distilled && f.Origin != fact.Discovered {
				continue
			}
			localRefs := localEvidenceRefs(f)
			if len(localRefs) == 0 {
				continue
			}
			w := synthesize.ComputeEvidenceWeight(ctx, s.facts, agentBranch, localRefs)
			if w <= 0 || w == f.EvidenceWeight {
				continue
			}
			f.EvidenceWeight = w
			facts[i] = f
			serialized, err := fact.SerializeFact(f)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: serialize weighted: %v", i, err)), nil
			}
			files[f.Path()] = serialized
		}

		// Build the precomputed-embedding donation map keyed by final on-disk
		// path. upsert reads this from ctx and skips its own ONNX call when an
		// entry is present. Empty/missing entries fall through to embedding.
		embByPath := make(map[string][]float32, len(facts))
		for i := range donatePaths {
			if donatePaths[i] == "" {
				continue
			}
			if i >= len(dedupVecs) || len(dedupVecs[i]) == 0 {
				continue
			}
			embByPath[donatePaths[i]] = dedupVecs[i]
		}
		ctx = store.WithPrecomputedEmbeddings(ctx, embByPath)

		// 4. BatchWrite all facts — and any subsumed hypotheses' retractions —
		// in one commit, so a learn call is all-or-nothing.
		commitMsg := fmt.Sprintf("learn: %s", momentName)
		hash, _, err := s.facts.BatchWriteFacts(ctx, agentBranch, files, retract, commitMsg, "learn")
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		// 5. Build response.
		type commitEntry struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		}
		commits := make([]commitEntry, len(facts))
		for i, f := range facts {
			commits[i] = commitEntry{File: f.Path(), Hash: hash}
		}

		result := map[string]interface{}{
			"commits": commits,
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
