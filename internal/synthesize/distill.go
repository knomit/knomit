package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/cluster"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

// DistillResult is the LLM JSON response for a distill step.
type DistillResult struct {
	Synthesize []distillFact `json:"synthesize"`
	Forget     []string      `json:"forget"`
}

// distillFact is a synthesized fact returned by the LLM in a distill step.
type distillFact struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
}

// buildDistillPrompt builds the LLM prompt for a distill step.
func buildDistillPrompt(facts []factForLLM, recipePrompt, stepPrompt string) string {
	factsJSON, _ := json.MarshalIndent(facts, "", "  ")

	var sb strings.Builder
	sb.WriteString("You are synthesizing facts in a knowledge base to find patterns and higher-order insights.\n\n")
	if recipePrompt != "" {
		sb.WriteString("Context: ")
		sb.WriteString(recipePrompt)
		sb.WriteString("\n")
	}
	if stepPrompt != "" {
		sb.WriteString("Instructions: ")
		sb.WriteString(stepPrompt)
		sb.WriteString("\n")
	}
	sb.WriteString("Facts in scope:\n")
	sb.Write(factsJSON)
	sb.WriteString(`

Identify patterns across these facts. Produce:
1. New higher-order facts that capture patterns
2. Which original facts are fully subsumed and can be forgotten

Respond as JSON (no markdown wrapping):
{
  "synthesize": [
    {
      "path": "know/...",
      "title": "...",
      "body": "...",
      "domain": [],
      "confidence": 0.X,
      "entities": [],
      "refs": ["source-file1.md", "source-file2.md"]
    }
  ],
  "forget": ["file1.md", "file2.md"]
}`)
	return sb.String()
}

// parseDistillResponse parses the LLM JSON response for a distill step.
func parseDistillResponse(text string) (DistillResult, error) {
	raw := extractJSON(text)
	var result DistillResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return DistillResult{}, fmt.Errorf("parseDistillResponse: %w (raw: %.200s)", err, raw)
	}
	return result, nil
}

// executeDistillStep runs one distill (RAPTOR) step of the synthesis pipeline.
func executeDistillStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, onProgress func(ProgressEvent)) error {
	maxDepth := step.MaxDepth
	if maxDepth == 0 {
		maxDepth = 1
	}
	umapDims := step.UMAPDimensions
	if umapDims == 0 {
		umapDims = 5
	}
	minCluster := step.MinClusterSize
	if minCluster == 0 {
		minCluster = 3
	}

	log.Debug().Int("max_depth", maxDepth).Int("umap_dims", umapDims).Int("min_cluster", minCluster).Msg("distill: config")

	// Gather initial facts from the index (all facts, no filter).
	searchResults, err := idx.Search(store.SearchQuery{Limit: 100_000})
	if err != nil {
		return fmt.Errorf("distill: search all: %w", err)
	}
	log.Debug().Int("facts", len(searchResults)).Msg("distill: gathered facts from index")
	if len(searchResults) == 0 {
		onProgress(ProgressEvent{Phase: "distill", Message: "no facts in index"})
		return nil
	}

	// Build current working set from search results.
	type workFact struct {
		factForLLM
		embedding []float32
	}
	buildWorkFacts := func(results []store.SearchResult) []workFact {
		wf := make([]workFact, 0, len(results))
		for _, r := range results {
			wf = append(wf, workFact{
				factForLLM: factForLLM{
					File:       r.Path,
					Title:      r.Title,
					Body:       r.Body,
					Domain:     r.Domain,
					Entities:   r.Entities,
					Confidence: r.Confidence,
					Sources:    r.Sources,
				},
			})
		}
		return wf
	}

	currentFacts := buildWorkFacts(searchResults)

	// Load embeddings for the initial fact set.
	for i, f := range currentFacts {
		if embedder == nil {
			break
		}
		gei, ok := idx.(interface{ GetEmbedding(string) ([]float32, error) })
		if !ok {
			break
		}
		vec, err := gei.GetEmbedding(f.File)
		if err != nil || vec == nil {
			continue
		}
		currentFacts[i].embedding = vec
	}

	var allSynthesized []distillFact
	allForget := map[string]bool{}

	for depth := 0; depth < maxDepth; depth++ {
		log.Debug().Int("depth", depth+1).Int("max_depth", maxDepth).Int("facts", len(currentFacts)).Msg("distill: RAPTOR depth")
		onProgress(ProgressEvent{Phase: "raptor-depth", Message: fmt.Sprintf("%d/%d", depth+1, maxDepth)})

		// Build embedding matrix: only facts that have embeddings.
		type indexedFact struct {
			idx  int
			fact workFact
		}
		var withEmbedding []indexedFact
		for i, f := range currentFacts {
			if len(f.embedding) > 0 {
				withEmbedding = append(withEmbedding, indexedFact{i, f})
			}
		}

		// If not enough facts have embeddings, send all facts as one cluster.
		if len(withEmbedding) < minCluster {
			onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("insufficient embeddings (%d), using single cluster", len(withEmbedding))})
			group := make([]factForLLM, len(currentFacts))
			for i, f := range currentFacts {
				group[i] = f.factForLLM
			}
			synthesized, forget, err := runDistillOnGroup(ctx, gs, idx, adapter, group, step, recipe, onProgress)
			if err != nil {
				return err
			}
			allSynthesized = append(allSynthesized, synthesized...)
			for _, p := range forget {
				allForget[p] = true
			}
			break
		}

		// Build float64 vectors for UMAP+HDBSCAN.
		vecs := make([][]float64, len(withEmbedding))
		for i, wf := range withEmbedding {
			v32 := wf.fact.embedding
			v64 := make([]float64, len(v32))
			for j, x := range v32 {
				v64[j] = float64(x)
			}
			vecs[i] = v64
		}

		// UMAP dimensionality reduction.
		reduced, err := cluster.UMAP(vecs, cluster.UMAPOptions{
			NComponents: umapDims,
			NNeighbors:  15,
			MinDist:     0.1,
		})
		if err != nil {
			// Fall back to raw vectors if UMAP fails.
			reduced = vecs
		}

		// HDBSCAN clustering.
		labels := cluster.HDBSCAN(reduced, cluster.HDBSCANOptions{
			MinClusterSize: minCluster,
		})

		// Group facts by cluster label (skip noise = -1).
		clusterMap := map[int][]factForLLM{}
		for i, wf := range withEmbedding {
			label := labels[i]
			if label == -1 {
				continue
			}
			clusterMap[label] = append(clusterMap[label], wf.fact.factForLLM)
		}

		onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("%d clusters", len(clusterMap))})

		if len(clusterMap) == 0 {
			// No clusters formed; stop RAPTOR recursion.
			break
		}

		var depthSynthesized []distillFact
		for _, group := range clusterMap {
			synthesized, forget, err := runDistillOnGroup(ctx, gs, idx, adapter, group, step, recipe, onProgress)
			if err != nil {
				return err
			}
			depthSynthesized = append(depthSynthesized, synthesized...)
			for _, p := range forget {
				allForget[p] = true
			}
		}
		allSynthesized = append(allSynthesized, depthSynthesized...)

		// RAPTOR recursion: re-embed new synthesized facts for next depth.
		if depth+1 < maxDepth && len(depthSynthesized) > 0 && embedder != nil {
			nextFacts := make([]workFact, 0, len(depthSynthesized))
			for _, df := range depthSynthesized {
				embText := fmt.Sprintf("%s %s %s %s",
					df.Title, df.Body,
					strings.Join(df.Entities, " "),
					strings.Join(df.Domain, " "),
				)
				vec, err := embedder.Embed(embText)
				if err != nil {
					vec = nil
				}
				nextFacts = append(nextFacts, workFact{
					factForLLM: factForLLM{
						File:       df.Path,
						Title:      df.Title,
						Body:       df.Body,
						Domain:     df.Domain,
						Entities:   df.Entities,
						Confidence: df.Confidence,
						Sources:    1,
					},
					embedding: vec,
				})
			}
			currentFacts = nextFacts
		} else {
			break
		}
	}

	log.Info().Int("synthesized", len(allSynthesized)).Int("forgotten", len(allForget)).Msg("distill: committing results")

	// Commit synthesized facts.
	for _, df := range allSynthesized {
		fact := mcp.Fact{
			Path:       df.Path,
			Title:      df.Title,
			Body:       df.Body,
			Domain:     df.Domain,
			Confidence: df.Confidence,
			Sources:    1,
			Entities:   df.Entities,
			Refs:       df.Refs,
		}
		content := mcp.SerializeFact(fact)
		msg := fmt.Sprintf("synthesize-%s: distill %s", recipe.Name, df.Path)
		if err := gs.WriteFile(df.Path, content, msg); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill write %s: %v", df.Path, err)})
			continue
		}
		head, _ := gs.HeadCommit()
		_ = idx.Upsert(store.FactRecord{
			Path:       df.Path,
			Title:      df.Title,
			Body:       df.Body,
			Domain:     df.Domain,
			Entities:   df.Entities,
			Confidence: df.Confidence,
			Sources:    1,
			Refs:       df.Refs,
			CommitHash: head,
		})
		onProgress(ProgressEvent{Phase: "detail-learn", Message: df.Path})
	}

	// Delete subsumed facts.
	for path := range allForget {
		msg := fmt.Sprintf("synthesize-%s: subsumed by distilled fact", recipe.Name)
		if err := gs.DeleteFile(path, msg); err != nil {
			onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("distill forget %s: %v", path, err)})
			continue
		}
		_ = idx.Delete(path)
		onProgress(ProgressEvent{Phase: "detail-distill-forget", Message: path})
	}

	tagName := fmt.Sprintf("learn/synthesize-%s-distill", recipe.Name)
	if err := gs.Tag(tagName); err != nil {
		onProgress(ProgressEvent{Phase: "warn", Message: fmt.Sprintf("tag %s: %v", tagName, err)})
	}

	onProgress(ProgressEvent{Phase: "distill-done", Message: fmt.Sprintf("%d synthesized, %d forgotten", len(allSynthesized), len(allForget))})
	return nil
}

// runDistillOnGroup sends one group of facts to the LLM and returns synthesized facts + paths to forget.
func runDistillOnGroup(ctx context.Context, gs GitStore, idx SearchIndex, adapter llm.LLMAdapter, group []factForLLM, step RecipeStep, recipe Recipe, onProgress func(ProgressEvent)) ([]distillFact, []string, error) {
	const maxChunkBytes = 100_000
	chunks := chunkFacts(group, maxChunkBytes)

	var synthesized []distillFact
	var forget []string

	for i, chunk := range chunks {
		log.Debug().Int("chunk", i+1).Int("total", len(chunks)).Int("facts", len(chunk)).Msg("distill: sending to LLM")
		onProgress(ProgressEvent{Phase: "llm", Message: fmt.Sprintf("distill chunk %d/%d (%d facts)", i+1, len(chunks), len(chunk))})
		prompt := buildDistillPrompt(chunk, recipe.Prompt, step.Prompt)
		response, err := adapter.Complete(
			ctx,
			"You are a knowledge base synthesis assistant. Respond only with valid JSON.",
			[]llm.Message{{Role: "user", Content: prompt}},
			nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("distill LLM chunk %d: %w", i+1, err)
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, nil, fmt.Errorf("distill parse chunk %d: %w", i+1, err)
		}
		log.Debug().Int("synthesized", len(result.Synthesize)).Int("forget", len(result.Forget)).Msg("distill: LLM response parsed")
		synthesized = append(synthesized, result.Synthesize...)
		forget = append(forget, result.Forget...)
	}
	return synthesized, forget, nil
}
