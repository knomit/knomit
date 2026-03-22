package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/fact"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

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
					"body":       map[string]any{"type": "string", "description": "Fact body in natural language."},
					"type":       map[string]any{"type": "string", "description": "Epistemic type: observation (default, concrete facts), concept (definitions), process (procedures), principle (rules/heuristics), pattern (recurring structures), reference (specs/measurements), synthesis (derived from other facts), hypothesis (predictions from patterns — carries uncertainty), methodology (reasoning process lessons).", "default": "observation"},
					"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Cross-cutting domain tags."},
					"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0.", "default": 0.7},
					"sources":    map[string]any{"type": "integer", "description": "Number of independent sources.", "default": 1},
					"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Entities this fact mentions."},
					"refs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "External URLs or source references."},
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
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
}


// BatchEmbedder computes embedding vectors for multiple texts in a single call.
type BatchEmbedder interface {
	Embed(text string) ([]float32, error)
	EmbedBatch(texts []string) ([][]float32, error)
}

// LearnHandler returns the handler function for knomit_learn.
// If embedder is non-nil, dedup checks batch-embed all incoming facts upfront
// instead of embedding one-at-a-time inside each Search call.
func LearnHandler(gs GitStore, idx SearchIndex, ontologyRoot string, ontology *fact.Ontology, embedders ...BatchEmbedder) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	var batchEmb BatchEmbedder
	if len(embedders) > 0 {
		batchEmb = embedders[0]
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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
			eType := fact.EpistemicType(fi.Type)
			if eType == "" {
				eType = fact.DefaultType
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
		facts := make([]Fact, len(factInputs))
		for i, fi := range factInputs {
			// Validate topic+category against ontology.
			topicCategory := fi.Topic
			if fi.Category != "" {
				topicCategory = fi.Topic + "/" + fi.Category
			}
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
			// Validate epistemic type.
			eType := fact.EpistemicType(fi.Type)
			if eType == "" {
				eType = fact.DefaultType
			}
			if err := eType.Validate(); err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("fact %d: %v", i, err)), nil
			}
			f := Fact{
				Path:       path,
				Title:      fi.Title,
				Body:       fi.Body,
				Type:       eType,
				Domain:     domain,
				Confidence: fi.Confidence,
				Sources:    fi.Sources,
				Entities:   entities,
				Refs:       refs,
			}
			facts[i] = f
			files[path] = SerializeFact(f)
		}

		// 3b. Dedup check: search for near-duplicates scoped to the same category directory.
		// Batch-embed all incoming facts upfront if a BatchEmbedder is available,
		// so each dedup Search uses the pre-computed vector instead of re-embedding.
		const dedupThreshold = 0.92
		var dedupVecs [][]float32
		if batchEmb != nil && len(facts) > 0 {
			texts := make([]string, len(facts))
			for i, f := range facts {
				texts[i] = f.Title + " " + f.Body
			}
			dedupVecs, _ = batchEmb.EmbedBatch(texts)
		}
		for i, f := range facts {
			categoryDir := f.Path[:strings.LastIndex(f.Path, "/")]
			sq := SearchQuery{
				Text:          f.Title + " " + f.Body,
				Path:          categoryDir,
				MinSimilarity: dedupThreshold,
				Limit:         1,
			}
			if dedupVecs != nil && i < len(dedupVecs) && len(dedupVecs[i]) > 0 {
				sq.QueryVec = dedupVecs[i]
			}
			results, err := idx.Search(sq)
			if err != nil || len(results) == 0 {
				continue
			}

			match := results[0]
			// Read existing fact to get its full metadata (refs, etc.)
			existingContent, readErr := gs.ReadFile(match.Path)
			if readErr != nil {
				continue
			}
			existingFact, parseErr := ParseFact(match.Path, existingContent)
			if parseErr != nil {
				continue
			}

			// Type-aware dedup: if existing fact is a hypothesis and new fact is not,
			// the observation subsumes the hypothesis.
			if existingFact.Type == fact.Hypothesis && f.Type != fact.Hypothesis {
				// Write the observation as normal (don't merge into existing path).
				// Retract the hypothesis.
				retractMsg := fmt.Sprintf("learn: hypothesis %s subsumed by observation", match.Path)
				gs.DeleteFile(match.Path, retractMsg, "retract")
				// Add hypothesis path to observation's refs.
				f.Refs = fact.AppendUnique(f.Refs, match.Path)
				facts[i] = f
				files[f.Path] = SerializeFact(f)
				continue
			}

			// Determine winner by merge rule: higher confidence wins, tie-break by sources.
			newConf := f.Confidence
			existConf := existingFact.Confidence

			var merged Fact
			if newConf > existConf || (newConf == existConf && f.Sources >= existingFact.Sources) {
				// New fact wins — keep new fact's title and body, write to existing path.
				merged = Fact{
					Path:       match.Path,
					Title:      f.Title,
					Body:       f.Body,
					Type:       f.Type,
					Domain:     fact.UnionStrings(f.Domain, existingFact.Domain),
					Entities:   fact.UnionStrings(f.Entities, existingFact.Entities),
					Confidence: max(newConf, existConf),
					Sources:    f.Sources + existingFact.Sources,
					Refs:       fact.AppendUnique(fact.UnionStrings(f.Refs, existingFact.Refs), match.Path),
				}
			} else {
				// Existing fact wins — keep existing title and body, update metadata.
				merged = Fact{
					Path:       match.Path,
					Title:      existingFact.Title,
					Body:       existingFact.Body,
					Type:       existingFact.Type,
					Domain:     fact.UnionStrings(f.Domain, existingFact.Domain),
					Entities:   fact.UnionStrings(f.Entities, existingFact.Entities),
					Confidence: max(newConf, existConf),
					Sources:    f.Sources + existingFact.Sources,
					Refs:       fact.AppendUnique(fact.UnionStrings(f.Refs, existingFact.Refs), match.Path),
				}
			}

			// Remove the original new-fact path from the files map and add the merged one.
			delete(files, f.Path)
			files[merged.Path] = SerializeFact(merged)
			facts[i] = merged
		}

		// 4. BatchWrite all facts in one commit.
		commitMsg := fmt.Sprintf("learn: %s", momentName)
		hash, _, err := gs.BatchWrite(files, commitMsg, "learn")
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
			commits[i] = commitEntry{File: f.Path, Hash: hash}
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
