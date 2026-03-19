// Package synthesize — Review orchestrator: multi-turn review sessions that
// connect clustering, prompt rendering, decision application, and session storage.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

// ReviewIndex is the interface for review session storage (subset of store.Index).
type ReviewIndex interface {
	GetReviewWatermark(branch string) (string, error)
	SetReviewWatermark(branch, hash string) error
	CreateReviewSession(branch string) (*store.ReviewSession, error)
	GetReviewSession(id string) (*store.ReviewSession, error)
	CompleteReviewSession(id string) error
	InsertWorkItem(item store.ReviewWorkItem) error
	NextWorkItem(sessionID string) (*store.ReviewWorkItem, error)
	SetWorkItemResponse(id int64, response string) error
	WorkItemStats(sessionID string) (completed, remaining int, err error)
	GCReviewSessions(branch string, keep int) error
}

// Reviewer orchestrates multi-turn review sessions.
type Reviewer struct {
	gs         GitStore
	idx        SearchIndex
	reviewIdx  ReviewIndex
	embedder   Embedder
	onProgress func(ProgressEvent)
}

// NewReviewer creates a new review orchestrator.
func NewReviewer(gs GitStore, idx SearchIndex, reviewIdx ReviewIndex, embedder Embedder, onProgress func(ProgressEvent)) *Reviewer {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	return &Reviewer{gs: gs, idx: idx, reviewIdx: reviewIdx, embedder: embedder, onProgress: onProgress}
}

// StartSession creates a new review session, identifies dirty facts, clusters
// them, stores work items, and returns the first item to review.
func (r *Reviewer) StartSession() (*mcp.ReviewResult, error) {
	branch := r.gs.Branch()

	// GC old sessions.
	if err := r.reviewIdx.GCReviewSessions(branch, 5); err != nil {
		log.Warn().Err(err).Msg("review: GC old sessions failed")
	}

	sess, err := r.reviewIdx.CreateReviewSession(branch)
	if err != nil {
		return nil, fmt.Errorf("review: create session: %w", err)
	}

	seeds, err := r.dirtyFacts(branch)
	if err != nil {
		return nil, fmt.Errorf("review: dirty facts: %w", err)
	}

	if len(seeds) == 0 {
		return r.completeSession(sess)
	}

	// Build scoped clusters.
	clusters, err := ScopedCluster(seeds, r.idx, 1.0, r.onProgress)
	if err != nil {
		return nil, fmt.Errorf("review: cluster: %w", err)
	}

	// Dedup pass: merge near-duplicates within each cluster before enqueueing.
	for i := range clusters {
		surviving, err := dedupCluster(context.Background(), clusters[i], r.gs, r.idx, 0.92, "review", r.onProgress)
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
		item := store.ReviewWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("cluster-%d", i),
			FactsJSON:  string(factsJSON),
			Priority:   float64(len(cluster)),
		}
		if err := r.reviewIdx.InsertWorkItem(item); err != nil {
			return nil, fmt.Errorf("review: insert prune item: %w", err)
		}
	}

	// Store one distill work item if >1 seed (lower priority than prune).
	if len(seeds) > 1 {
		factsJSON, err := json.Marshal(seeds)
		if err != nil {
			return nil, fmt.Errorf("review: marshal seeds for distill: %w", err)
		}
		item := store.ReviewWorkItem{
			SessionID:  sess.ID,
			StepType:   "distill",
			ClusterKey: "distill-all",
			FactsJSON:  string(factsJSON),
			Priority:   0.0,
		}
		if err := r.reviewIdx.InsertWorkItem(item); err != nil {
			return nil, fmt.Errorf("review: insert distill item: %w", err)
		}
	}

	log.Info().Str("session", sess.ID).Int("clusters", len(pruneClusters)).Int("seeds", len(seeds)).Msg("review: session started")
	r.onProgress(ProgressEvent{Phase: "review-start", Message: fmt.Sprintf("session %s: %d clusters, %d seeds", sess.ID, len(pruneClusters), len(seeds))})

	return r.nextItem(sess.ID)
}

// ContinueSession processes the model's response for the current work item
// and returns the next item, or done if the session is complete.
func (r *Reviewer) ContinueSession(sessionID, response string) (*mcp.ReviewResult, error) {
	sess, err := r.reviewIdx.GetReviewSession(sessionID)
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
	item, err := r.reviewIdx.NextWorkItem(sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: next work item: %w", err)
	}
	if item == nil {
		// No unanswered items — session should be done already.
		return r.completeSession(sess)
	}

	// Parse the facts from the work item.
	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		return nil, fmt.Errorf("review: unmarshal facts: %w", err)
	}
	inputPaths := make([]string, len(facts))
	for i, f := range facts {
		inputPaths[i] = f.File
	}

	// Apply based on step type.
	switch item.StepType {
	case "prune":
		result, err := parsePruneResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse prune response: %w", err)
		}
		if err := validatePrunePaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate prune: %w", err)
		}
		if _, err := ApplyPruneDecisions(r.gs, r.idx, result.Decisions, result.Merges, "review", r.onProgress); err != nil {
			return nil, fmt.Errorf("review: apply prune: %w", err)
		}

	case "distill":
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse distill response: %w", err)
		}
		if err := validateDistillPaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate distill: %w", err)
		}
		_, writtenFacts, err := ApplyDistillDecisions(r.gs, r.idx, result.Synthesize, result.Retract, "review", r.onProgress)
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
			raptorClusters, clErr := ScopedCluster(newFacts, r.idx, 1.0, r.onProgress)
			if clErr != nil {
				log.Warn().Err(clErr).Msg("review: RAPTOR clustering failed")
			} else {
				nextDepth := item.Depth + 1
				for ci, cluster := range raptorClusters {
					factsJSON, _ := json.Marshal(cluster)
					wItem := store.ReviewWorkItem{
						SessionID:  sessionID,
						StepType:   "distill",
						ClusterKey: fmt.Sprintf("raptor-d%d-c%d", nextDepth, ci),
						FactsJSON:  string(factsJSON),
						Priority:   float64(-nextDepth),
						Depth:      nextDepth,
					}
					if err := r.reviewIdx.InsertWorkItem(wItem); err != nil {
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
	if err := r.reviewIdx.SetWorkItemResponse(item.ID, response); err != nil {
		return nil, fmt.Errorf("review: set response: %w", err)
	}

	return r.nextItem(sessionID)
}

// dirtyFacts returns seed facts (changed since watermark).
//
// First run (no watermark): uses the search index to retrieve all facts
// without reading every file from git.
// Incremental (has watermark): uses DiffFiles to read only changed paths.
func (r *Reviewer) dirtyFacts(branch string) ([]factForLLM, error) {
	watermark, err := r.reviewIdx.GetReviewWatermark(branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	// No watermark → first run, all facts are dirty. Use the index (fast).
	if watermark == "" {
		results, err := r.idx.Search(store.SearchQuery{Limit: 100_000})
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
	added, modified, _, err := r.gs.DiffFiles(watermark)
	if err != nil {
		return nil, fmt.Errorf("diff files: %w", err)
	}

	var seeds []factForLLM
	for _, path := range append(added, modified...) {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		content, err := r.gs.ReadFile(path)
		if err != nil {
			continue // deleted or unreadable
		}
		fact, err := mcp.ParseFact(path, content)
		if err != nil {
			continue // not a valid fact
		}
		seeds = append(seeds, factForLLM{
			File:       fact.Path,
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
func (r *Reviewer) nextItem(sessionID string) (*mcp.ReviewResult, error) {
	item, err := r.reviewIdx.NextWorkItem(sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: next item: %w", err)
	}

	if item == nil {
		sess, err := r.reviewIdx.GetReviewSession(sessionID)
		if err != nil {
			return nil, fmt.Errorf("review: get session for complete: %w", err)
		}
		return r.completeSession(sess)
	}

	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		return nil, fmt.Errorf("review: unmarshal facts for prompt: %w", err)
	}

	var content *WorkItemContent
	switch item.StepType {
	case "prune":
		content, err = RenderPruneWorkItem(facts)
	case "distill":
		content, err = RenderDistillWorkItem(facts)
	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}
	if err != nil {
		return nil, fmt.Errorf("review: render %s prompt: %w", item.StepType, err)
	}

	completed, remaining, err := r.reviewIdx.WorkItemStats(sessionID)
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
func (r *Reviewer) completeSession(sess *store.ReviewSession) (*mcp.ReviewResult, error) {
	if err := r.reviewIdx.CompleteReviewSession(sess.ID); err != nil {
		return nil, fmt.Errorf("review: complete session: %w", err)
	}

	headHash, err := r.gs.HeadCommit()
	if err != nil {
		log.Warn().Err(err).Msg("review: could not get HEAD for watermark")
	} else {
		if err := r.reviewIdx.SetReviewWatermark(sess.Branch, headHash); err != nil {
			log.Warn().Err(err).Msg("review: could not advance watermark")
		}
	}

	completed, _, err := r.reviewIdx.WorkItemStats(sess.ID)
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
