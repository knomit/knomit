package web

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"knomit/internal/embeddings"
	"knomit/internal/llm"
	"knomit/internal/synthesize"
)

// synthRun tracks a single synthesis run.
type synthRun struct {
	mu     sync.Mutex
	events []string
	done   bool
}

// LiveSynthRunner implements SynthRunner by launching synthesize.Run in a goroutine.
type LiveSynthRunner struct {
	mu       sync.Mutex
	runs     map[string]*synthRun
	gs       synthesize.GitStore
	idx      synthesize.SearchIndex
	embedder *embeddings.Embedder
	adapter  llm.LLMAdapter
	counter  int
}

// NewSynthRunner creates a LiveSynthRunner with the required dependencies.
func NewSynthRunner(gs synthesize.GitStore, idx synthesize.SearchIndex, embedder *embeddings.Embedder, adapter llm.LLMAdapter) *LiveSynthRunner {
	return &LiveSynthRunner{
		runs:     make(map[string]*synthRun),
		gs:       gs,
		idx:      idx,
		embedder: embedder,
		adapter:  adapter,
	}
}

// Start parses the recipe and launches a synthesis run in the background.
func (sr *LiveSynthRunner) Start(recipeYAML string) (string, error) {
	if sr.adapter == nil {
		return "", fmt.Errorf("LLM adapter not configured")
	}

	// Use default recipe if none provided.
	if recipeYAML == "" {
		recipeYAML = defaultRecipe
	}

	recipe, err := synthesize.ParseRecipe(recipeYAML)
	if err != nil {
		return "", fmt.Errorf("parse recipe: %w", err)
	}

	sr.mu.Lock()
	sr.counter++
	id := fmt.Sprintf("run-%d", sr.counter)
	run := &synthRun{}
	sr.runs[id] = run
	sr.mu.Unlock()

	log.Info().Str("id", id).Str("recipe", recipe.Name).Msg("synth runner: starting run")

	var emb synthesize.Embedder
	if sr.embedder != nil {
		emb = sr.embedder
	}

	go func() {
		onProgress := func(ev synthesize.ProgressEvent) {
			msg := fmt.Sprintf("[%s] %s", ev.Phase, ev.Message)
			log.Debug().Str("id", id).Str("phase", ev.Phase).Str("message", ev.Message).Msg("synth runner: progress")
			run.mu.Lock()
			run.events = append(run.events, msg)
			run.mu.Unlock()
		}

		err := synthesize.Run(context.Background(), sr.gs, sr.idx, emb, sr.adapter, recipe, onProgress)
		run.mu.Lock()
		if err != nil {
			log.Error().Err(err).Str("id", id).Msg("synth runner: run failed")
			run.events = append(run.events, fmt.Sprintf("[error] %v", err))
		} else {
			log.Info().Str("id", id).Msg("synth runner: run complete")
		}
		run.done = true
		run.mu.Unlock()
	}()

	return id, nil
}

// Status returns the accumulated events and whether the run is done.
func (sr *LiveSynthRunner) Status(id string) ([]string, bool) {
	sr.mu.Lock()
	run, ok := sr.runs[id]
	sr.mu.Unlock()
	if !ok {
		return nil, true
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	events := make([]string, len(run.events))
	copy(events, run.events)
	return events, run.done
}

const defaultRecipe = `name: default
prompt: Review and consolidate the knowledge base.
steps:
  - mode: prune
    prompt: Identify stale, redundant, or outdated facts.
`
