// Package synthesize — Review orchestrator: multi-turn review sessions that
// connect clustering, prompt rendering, decision application, and session storage.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// Reviewer orchestrates multi-turn review sessions.
type Reviewer struct {
	gs             store.FactIndex
	idx            store.SearchIndex
	reviewIdx      store.PipelineIndex
	embedder       store.Embedder
	onProgress     func(ProgressEvent)
	reflectChecked map[string]bool
	agentBranch    string
}

// NewReviewer creates a new review orchestrator.
func NewReviewer(gs store.FactIndex, idx store.SearchIndex, reviewIdx store.PipelineIndex, embedder store.Embedder, onProgress func(ProgressEvent), agentBranch string) *Reviewer {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	return &Reviewer{gs: gs, idx: idx, reviewIdx: reviewIdx, embedder: embedder, onProgress: onProgress, reflectChecked: make(map[string]bool), agentBranch: agentBranch}
}

// StartSession creates a new review session, identifies dirty facts, clusters
// them, stores work items, and returns the first item to review.
func (r *Reviewer) StartSession(ctx context.Context) (*mcp.ReviewResult, error) {
	branch := r.agentBranch

	sess, err := r.reviewIdx.CreatePipelineSession(ctx, "review", branch)
	if err != nil {
		return nil, fmt.Errorf("review: create session: %w", err)
	}

	seeds, err := r.dirtyFacts(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("review: dirty facts: %w", err)
	}

	if len(seeds) == 0 {
		return r.completeSession(ctx, sess)
	}

	// Build scoped clusters.
	clusters, err := ScopedCluster(ctx, seeds, r.idx, 1.0, r.onProgress, r.agentBranch)
	if err != nil {
		return nil, fmt.Errorf("review: cluster: %w", err)
	}

	// Dedup pass: merge near-duplicates within each cluster before enqueueing.
	for i := range clusters {
		surviving, err := dedupCluster(context.Background(), clusters[i], r.gs, r.idx, 0.92, "review", r.onProgress, r.agentBranch, r.embedder)
		if err != nil {
			return nil, fmt.Errorf("review: dedup cluster %d: %w", i, err)
		}
		clusters[i] = surviving
	}
	// Filter to clusters with > 1 fact (nothing for LLM to reason about with just 1).
	var pruneClusters [][]factForLLM
	for _, c := range clusters {
		if len(c) > 1 {
			pruneClusters = append(pruneClusters, c)
		}
	}

	// Store prune work items — priority = cluster size (bigger = more urgent).
	for i, cluster := range pruneClusters {
		factsJSON, err := json.Marshal(cluster)
		if err != nil {
			return nil, fmt.Errorf("review: marshal cluster %d: %w", i, err)
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("cluster-%d", i),
			FactsJSON:  string(factsJSON),
			Priority:   float64(len(cluster)),
		}
		if err := r.reviewIdx.InsertPipelineWorkItem(ctx, item); err != nil {
			return nil, fmt.Errorf("review: insert prune item: %w", err)
		}
	}

	// Store one distill work item if >1 seed (lower priority than prune).
	if len(seeds) > 1 {
		factsJSON, err := json.Marshal(seeds)
		if err != nil {
			return nil, fmt.Errorf("review: marshal seeds for distill: %w", err)
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "distill",
			ClusterKey: "distill-all",
			FactsJSON:  string(factsJSON),
			Priority:   0.0,
		}
		if err := r.reviewIdx.InsertPipelineWorkItem(ctx, item); err != nil {
			return nil, fmt.Errorf("review: insert distill item: %w", err)
		}
	}

	log.Info().Str("session", sess.ID).Int("clusters", len(pruneClusters)).Int("seeds", len(seeds)).Msg("review: session started")
	r.onProgress(ProgressEvent{Phase: "review-start", Message: fmt.Sprintf("session %s: %d clusters, %d seeds", sess.ID, len(pruneClusters), len(seeds))})

	return r.nextItem(ctx, sess.ID)
}

// ContinueSession processes the model's response for the current work item
// and returns the next item, or done if the session is complete.
func (r *Reviewer) ContinueSession(ctx context.Context, sessionID, response string) (*mcp.ReviewResult, error) {
	sess, err := r.reviewIdx.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("review: session %q not found", sessionID)
	}
	if sess.Status != "active" {
		return nil, fmt.Errorf("review: session %q is %s, not active", sessionID, sess.Status)
	}

	// Get the current (unanswered) work item.
	item, err := r.reviewIdx.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: next work item: %w", err)
	}
	if item == nil {
		// No unanswered items — session should be done already.
		return r.completeSession(ctx, sess)
	}

	// Apply based on step type.
	switch item.StepType {
	case "reflect":
		// Reflect responses are informational — the agent writes methodology facts
		// via knomit_learn. No server-side action needed.

	case "prune":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts: %w", err)
		}
		inputPaths := make([]string, len(facts))
		for i, f := range facts {
			inputPaths[i] = f.File
		}
		result, err := parsePruneResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse prune response: %w", err)
		}
		if err := validatePrunePaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate prune: %w", err)
		}
		if _, err := ApplyPruneDecisions(ctx, r.gs, r.idx, result.Decisions, result.Merges, "review", r.onProgress, r.agentBranch); err != nil {
			return nil, fmt.Errorf("review: apply prune: %w", err)
		}

	case "distill":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts: %w", err)
		}
		inputPaths := make([]string, len(facts))
		for i, f := range facts {
			inputPaths[i] = f.File
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse distill response: %w", err)
		}
		if err := validateDistillPaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate distill: %w", err)
		}
		_, writtenFacts, err := ApplyDistillDecisions(ctx, r.gs, r.idx, result.Synthesize, result.Retract, "review", r.onProgress, r.agentBranch)
		if err != nil {
			return nil, fmt.Errorf("review: apply distill: %w", err)
		}

		// RAPTOR: if new facts were synthesized and we haven't hit max depth, enqueue deeper.
		const maxRaptorDepth = 3
		if len(writtenFacts) > 0 && item.Depth < maxRaptorDepth {
			// Build factForLLM from the written (normalized-path) facts for clustering.
			newFacts := make([]factForLLM, 0, len(writtenFacts))
			for _, df := range writtenFacts {
				newFacts = append(newFacts, factForLLM{
					File: df.Path, Title: df.Title, Body: df.Body,
					Type: df.Type, Domain: df.Domain, Entities: df.Entities,
					Confidence: df.Confidence, Sources: 1,
				})
			}

			// Cluster the new facts to find groups worth distilling further.
			raptorClusters, clErr := ScopedCluster(ctx, newFacts, r.idx, 1.0, r.onProgress, r.agentBranch, "hypothesis")
			if clErr != nil {
				log.Warn().Err(clErr).Msg("review: RAPTOR clustering failed")
			} else {
				nextDepth := item.Depth + 1
				for ci, cluster := range raptorClusters {
					factsJSON, _ := json.Marshal(cluster)
					wItem := store.PipelineWorkItem{
						SessionID:  sessionID,
						StepType:   "distill",
						ClusterKey: fmt.Sprintf("raptor-d%d-c%d", nextDepth, ci),
						FactsJSON:  string(factsJSON),
						Priority:   float64(-nextDepth),
						Depth:      nextDepth,
					}
					if err := r.reviewIdx.InsertPipelineWorkItem(ctx, wItem); err != nil {
						log.Warn().Err(err).Msg("review: RAPTOR enqueue failed")
					}
				}
				if len(raptorClusters) > 0 {
					log.Info().Int("depth", nextDepth).Int("clusters", len(raptorClusters)).Msg("review: RAPTOR enqueued deeper distill items")
				}
			}
		}

	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}

	// Mark item as answered.
	if err := r.reviewIdx.SetPipelineWorkItemResponse(ctx, item.ID, response); err != nil {
		return nil, fmt.Errorf("review: set response: %w", err)
	}

	return r.nextItem(ctx, sessionID)
}

// RunAll drives the review session to completion using an LLM adapter.
// It starts a session, then loops: render prompt → LLM call → apply response
// until all work items are processed.
func (r *Reviewer) RunAll(ctx context.Context, adapter llm.LLMAdapter) error {
	result, err := r.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("RunAll: start: %w", err)
	}
	if result.Done {
		return nil
	}

	sessionID := result.SessionID
	for result.Item != nil {
		r.onProgress(ProgressEvent{
			Phase:   "llm",
			Message: fmt.Sprintf("processing %s work item", result.Item.Type),
		})

		opts := llm.CompletionOptions{ForceJSON: true}
		response, err := adapter.Complete(ctx, "", []llm.Message{
			{Role: "user", Content: result.Item.Prompt},
		}, opts, nil)
		if err != nil {
			return fmt.Errorf("RunAll: LLM %s: %w", result.Item.Type, err)
		}

		result, err = r.ContinueSession(ctx, sessionID, response)
		if err != nil {
			return fmt.Errorf("RunAll: continue: %w", err)
		}
	}
	return nil
}

// dirtyFacts returns seed facts (changed since watermark).
//
// First run (no watermark): uses the search index to retrieve all facts
// without reading every file from git.
// Incremental (has watermark): uses DiffFiles to read only changed paths.
func (r *Reviewer) dirtyFacts(ctx context.Context, branch string) ([]factForLLM, error) {
	watermark, err := r.reviewIdx.GetPipelineWatermark(ctx, "review", branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	// No watermark → first run, all facts are dirty. Use the index (fast).
	if watermark == "" {
		results, err := r.idx.Search(ctx, branch, store.SearchQuery{Limit: 100_000})
		if err != nil {
			return nil, fmt.Errorf("search all: %w", err)
		}
		facts := make([]factForLLM, 0, len(results))
		for _, sr := range results {
			facts = append(facts, factForLLM{
				File:       sr.Path,
				Title:      sr.Title,
				Body:       sr.Body,
				Type:       sr.Type,
				Domain:     sr.Domain,
				Entities:   sr.Entities,
				Confidence: sr.Confidence,
				Sources:    sr.Sources,
			})
		}
		return facts, nil
	}

	// Incremental: only changed facts since watermark.
	added, modified, _, err := r.gs.DiffFiles(ctx, r.agentBranch, watermark)
	if err != nil {
		return nil, fmt.Errorf("diff files: %w", err)
	}

	var seeds []factForLLM
	for _, path := range append(added, modified...) {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		result, err := r.gs.ReadFact(ctx, r.agentBranch, path, nil)
		if err != nil {
			continue // deleted or unreadable
		}
		fact, err := fact.ParseFact(path, result.Content)
		if err != nil {
			continue // not a valid fact
		}
		seeds = append(seeds, factForLLM{
			File:       fact.Path(),
			Title:      fact.Title,
			Body:       fact.Body,
			Type:       string(fact.Type),
			Domain:     fact.Domain,
			Entities:   fact.Entities,
			Confidence: fact.Confidence,
			Sources:    fact.Sources,
		})
	}
	return seeds, nil
}

// nextItem fetches the next unanswered work item, renders its prompt, and
// returns a ReviewResult. If no items remain, completes the session.
func (r *Reviewer) nextItem(ctx context.Context, sessionID string) (*mcp.ReviewResult, error) {
	item, err := r.reviewIdx.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: next item: %w", err)
	}

	if item == nil {
		// Before completing, check if we should enqueue a reflect step.
		if !r.reflectChecked[sessionID] {
			r.reflectChecked[sessionID] = true
			transitions, tErr := r.findHypothesisTransitions(ctx, sessionID)
			if tErr != nil {
				log.Warn().Err(tErr).Msg("review: failed to find hypothesis transitions")
			}
			if len(transitions) > 0 {
				transJSON, _ := json.Marshal(transitions)
				reflectItem := store.PipelineWorkItem{
					SessionID:  sessionID,
					StepType:   "reflect",
					ClusterKey: "reflect",
					FactsJSON:  string(transJSON),
					Priority:   -100,
				}
				if err := r.reviewIdx.InsertPipelineWorkItem(ctx, reflectItem); err != nil {
					log.Warn().Err(err).Msg("review: failed to enqueue reflect item")
				} else {
					// Recurse to fetch the just-enqueued reflect item.
					return r.nextItem(ctx, sessionID)
				}
			}
		}
		sess, err := r.reviewIdx.GetPipelineSession(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("review: get session for complete: %w", err)
		}
		return r.completeSession(ctx, sess)
	}

	var content *WorkItemContent
	switch item.StepType {
	case "prune":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts for prompt: %w", err)
		}
		content, err = RenderPruneWorkItem(facts)
	case "distill":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts for prompt: %w", err)
		}
		content, err = RenderDistillWorkItem(facts)
	case "reflect":
		content, err = RenderReflectWorkItem([]byte(item.FactsJSON))
	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}
	if err != nil {
		return nil, fmt.Errorf("review: render %s prompt: %w", item.StepType, err)
	}

	completed, remaining, err := r.reviewIdx.PipelineWorkItemStats(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: work item stats: %w", err)
	}

	return &mcp.ReviewResult{
		SessionID: sessionID,
		Item: &mcp.ReviewItem{
			Type:           item.StepType,
			Prompt:         content.Prompt,
			ResponseSchema: content.ResponseSchema,
		},
		Progress: &mcp.ReviewProgress{
			Completed: completed,
			Remaining: remaining,
		},
	}, nil
}

// completeSession marks the session done and advances the watermark.
func (r *Reviewer) completeSession(ctx context.Context, sess *store.PipelineSession) (*mcp.ReviewResult, error) {
	if err := r.reviewIdx.CompletePipelineSession(ctx, sess.ID); err != nil {
		return nil, fmt.Errorf("review: complete session: %w", err)
	}

	headHash, err := r.gs.HeadCommit(ctx, r.agentBranch)
	if err != nil {
		log.Warn().Err(err).Msg("review: could not get HEAD for watermark")
	} else {
		if err := r.reviewIdx.SetPipelineWatermark(ctx, "review", sess.Branch, headHash); err != nil {
			log.Warn().Err(err).Msg("review: could not advance watermark")
		}
	}

	completed, _, err := r.reviewIdx.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		log.Warn().Err(err).Msg("review: could not get final stats")
	}

	log.Info().Str("session", sess.ID).Int("completed", completed).Msg("review: session complete")
	r.onProgress(ProgressEvent{Phase: "review-done", Message: fmt.Sprintf("session %s complete", sess.ID)})

	return &mcp.ReviewResult{
		SessionID: sess.ID,
		Done:      true,
		Summary:   &mcp.ReviewStats{},
		Progress:  &mcp.ReviewProgress{Completed: completed, Remaining: 0},
	}, nil
}

// hypothesisTransition records a change to a hypothesis fact during a review session.
type hypothesisTransition struct {
	Path         string `json:"path"`
	OriginalType string `json:"original_type"`
	Action       string `json:"action"` // "promoted", "retracted", "confidence-updated"
	Detail       string `json:"detail,omitempty"`
}

// findHypothesisTransitions detects hypothesis facts that were promoted,
// retracted, or had their confidence changed during the current review session.
func (r *Reviewer) findHypothesisTransitions(ctx context.Context, sessionID string) ([]hypothesisTransition, error) {
	sess, err := r.reviewIdx.GetPipelineSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}

	// Read the watermark set at the end of the previous session.
	// Since we haven't advanced it yet (that happens in completeSession),
	// all commits between here and HEAD are changes made during this session.
	watermark, err := r.reviewIdx.GetPipelineWatermark(ctx, "review", sess.Branch)
	if err != nil || watermark == "" {
		return nil, err
	}

	_, modified, deleted, err := r.gs.DiffFiles(ctx, r.agentBranch, watermark)
	if err != nil {
		return nil, err
	}

	var transitions []hypothesisTransition

	// Check deleted paths — were any hypotheses retracted?
	for _, path := range deleted {
		readResult, err := r.gs.ReadFact(ctx, r.agentBranch, path, &store.ReadFactOpts{AtCommit: watermark})
		if err != nil {
			continue
		}
		f, err := fact.ParseFact(path, readResult.Content)
		if err != nil {
			continue
		}
		if f.Type == fact.Hypothesis {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "retracted",
			})
		}
	}

	// Check modified paths — did any hypothesis change confidence or type?
	for _, path := range modified {
		oldResult, err := r.gs.ReadFact(ctx, r.agentBranch, path, &store.ReadFactOpts{AtCommit: watermark})
		if err != nil {
			continue
		}
		oldFact, err := fact.ParseFact(path, oldResult.Content)
		if err != nil || oldFact.Type != fact.Hypothesis {
			continue
		}
		newResult, err := r.gs.ReadFact(ctx, r.agentBranch, path, nil)
		if err != nil {
			continue
		}
		newFact, err := fact.ParseFact(path, newResult.Content)
		if err != nil {
			continue
		}
		if newFact.Type != oldFact.Type {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "promoted",
				Detail: fmt.Sprintf("type changed to %s", newFact.Type),
			})
		} else if newFact.Confidence != oldFact.Confidence {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "confidence-updated",
				Detail: fmt.Sprintf("%.2f → %.2f", oldFact.Confidence, newFact.Confidence),
			})
		}
	}

	return transitions, nil
}
