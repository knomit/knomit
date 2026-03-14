package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
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

// LearnHandler returns the handler function for knomit_learn.
func LearnHandler(gs GitStore, idx SearchIndex, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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

		// 3. Normalize paths and serialize facts.
		files := make(map[string]string, len(factInputs))
		facts := make([]Fact, len(factInputs))
		for i, fi := range factInputs {
			path := normalizePath(ontologyRoot, fi.Path)
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
			f := Fact{
				Path:       path,
				Title:      fi.Title,
				Body:       fi.Body,
				Domain:     domain,
				Confidence: fi.Confidence,
				Sources:    fi.Sources,
				Entities:   entities,
				Refs:       refs,
			}
			facts[i] = f
			files[path] = SerializeFact(f)
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
