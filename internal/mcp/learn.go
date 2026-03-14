package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"knomit/internal/fact"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// learnTool returns the Tool definition for knomit_learn.
func learnTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_learn",
		mcpgo.WithDescription("Write one or more facts to the knowledge base in a single commit."),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this learning moment (used as a git tag)."),
		),
		mcpgo.WithArray("facts",
			mcpgo.Required(),
			mcpgo.Description("Array of fact objects to write."),
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

// sanitizeMomentName replaces characters not in [a-zA-Z0-9._/-] with '-'.
var nonSafeRe = regexp.MustCompile(`[^a-zA-Z0-9._/\-]`)

func sanitizeMomentName(name string) string {
	return nonSafeRe.ReplaceAllString(name, "-")
}

// unionStrings returns the deduplicated union of two string slices, preserving order.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// appendUnique appends s to slice only if not already present.
func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

// normalizePath ensures the path starts with "<ontologyRoot>/" and ends with ".md".
func normalizePath(ontologyRoot, path string) string {
	prefix := ontologyRoot + "/"
	if !strings.HasPrefix(path, prefix) {
		path = prefix + path
	}
	if !strings.HasSuffix(path, ".md") {
		path = path + ".md"
	}
	return path
}

// buildFactPath constructs a fact file path: <ontologyRoot>/<topic>/<category>/<uuid>.md
func buildFactPath(ontologyRoot, topic, category string) string {
	id := uuid.New().String()[:8]
	category = strings.TrimPrefix(category, "/")
	category = strings.TrimSuffix(category, "/")
	return fmt.Sprintf("%s/%s/%s/%s.md", ontologyRoot, topic, category, id)
}

// LearnHandler returns the handler function for knomit_learn.
func LearnHandler(gs GitStore, idx SearchIndex, ontologyRoot string, ontology *fact.Ontology) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// 1. Sync.
		_, err := gs.Sync(nil)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("sync error: %v", err)), nil
		}

		// 2. Parse arguments.
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// Parse facts from the arguments.
		args := req.GetArguments()
		factsRaw, ok := args["facts"]
		if !ok {
			return mcpgo.NewToolResultError("facts is required"), nil
		}

		// Re-marshal and unmarshal to handle type coercion.
		factsJSON, err := json.Marshal(factsRaw)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("invalid facts: %v", err)), nil
		}
		var factInputs []learnFactInput
		if err := json.Unmarshal(factsJSON, &factInputs); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("invalid facts format: %v", err)), nil
		}
		if len(factInputs) == 0 {
			return mcpgo.NewToolResultError("facts must not be empty"), nil
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
			path := buildFactPath(ontologyRoot, fi.Topic, fi.Category)

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
		const dedupThreshold = 0.92
		for i, f := range facts {
			categoryDir := f.Path[:strings.LastIndex(f.Path, "/")]
			results, err := idx.Search(SearchQuery{
				Text:          f.Title + " " + f.Body,
				Path:          categoryDir,
				MinSimilarity: dedupThreshold,
				Limit:         1,
			})
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
					Domain:     unionStrings(f.Domain, existingFact.Domain),
					Entities:   unionStrings(f.Entities, existingFact.Entities),
					Confidence: max(newConf, existConf),
					Sources:    f.Sources + existingFact.Sources,
					Refs:       appendUnique(unionStrings(f.Refs, existingFact.Refs), match.Path),
				}
			} else {
				// Existing fact wins — keep existing title and body, update metadata.
				merged = Fact{
					Path:       match.Path,
					Title:      existingFact.Title,
					Body:       existingFact.Body,
					Type:       existingFact.Type,
					Domain:     unionStrings(f.Domain, existingFact.Domain),
					Entities:   unionStrings(f.Entities, existingFact.Entities),
					Confidence: max(newConf, existConf),
					Sources:    f.Sources + existingFact.Sources,
					Refs:       appendUnique(unionStrings(f.Refs, existingFact.Refs), match.Path),
				}
			}

			// Remove the original new-fact path from the files map and add the merged one.
			delete(files, f.Path)
			files[merged.Path] = SerializeFact(merged)
			facts[i] = merged
		}

		// 4. BatchWrite all facts in one commit.
		commitMsg := fmt.Sprintf("learn: %s", momentName)
		hash, blobHashes, err := gs.BatchWrite(files, commitMsg)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		// 5. Upsert each fact into index.
		for _, f := range facts {
			rec := FactRecord{
				Path:       f.Path,
				Title:      f.Title,
				BlobHash:   blobHashes[f.Path],
				Type:       string(f.Type),
				Domain:     f.Domain,
				Entities:   f.Entities,
				Confidence: f.Confidence,
				Sources:    f.Sources,
				Refs:       f.Refs,
				CommitHash: hash,
			}
			if err := idx.Upsert(rec); err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("index upsert error: %v", err)), nil
			}
		}

		// 7. Tag.
		sanitized := sanitizeMomentName(momentName)
		tagName := "learn/" + sanitized
		if err := gs.Tag(tagName); err != nil {
			// Tag already exists — append unix seconds.
			tagName = fmt.Sprintf("learn/%s-%d", sanitized, time.Now().Unix())
			if err2 := gs.Tag(tagName); err2 != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("tag error: %v", err2)), nil
			}
		}

		// 8. Build response.
		type commitEntry struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		}
		commits := make([]commitEntry, len(facts))
		for i, f := range facts {
			commits[i] = commitEntry{File: f.Path, Hash: hash}
		}

		result := map[string]interface{}{
			"moment_tag": tagName,
			"commits":    commits,
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
