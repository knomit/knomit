package synthesize

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
	"knomit/internal/store"
)

// GitStore is the interface that the synthesize pipeline requires from the git store.
type GitStore interface {
	ReadFile(path string) (string, error)
	WriteFile(path, content, message string) error
	BatchWrite(files map[string]string, message string) error
	DeleteFile(path, message string) error
	ListAll() ([]string, error)
	HeadCommit() (string, error)
	Tag(name string) error
	Branch() string
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
}

// SearchIndex is the interface that the synthesize pipeline requires from the search index.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	Upsert(r store.FactRecord) error
	Delete(path string) error
	Sync(g store.GitReader) error
	GetLastCommit() (string, error)
}

// ProgressEvent carries progress information from the pipeline to the caller.
type ProgressEvent struct {
	Phase   string
	Message string
}

// Embedder is the interface for computing embedding vectors.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// Run executes a synthesis recipe against the provided git store and search index.
// onProgress is called for each pipeline event; if nil, a no-op is used.
func Run(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, r Recipe, onProgress func(ProgressEvent)) error {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}

	log.Info().Str("recipe", r.Name).Int("steps", len(r.Steps)).Msg("synthesis: pipeline starting")

	for i, step := range r.Steps {
		log.Info().Str("mode", step.Mode).Int("step", i+1).Int("total", len(r.Steps)).Msg("synthesis: step starting")
		onProgress(ProgressEvent{Phase: "step-start", Message: step.Mode})
		var err error
		switch step.Mode {
		case "prune":
			err = executePruneStep(ctx, gs, idx, adapter, step, r, onProgress)
		case "distill":
			err = executeDistillStep(ctx, gs, idx, embedder, adapter, step, r, onProgress)
		default:
			return fmt.Errorf("unknown step mode: %q", step.Mode)
		}
		if err != nil {
			log.Error().Err(err).Str("mode", step.Mode).Msg("synthesis: step failed")
			return fmt.Errorf("step %q: %w", step.Mode, err)
		}
		log.Info().Str("mode", step.Mode).Msg("synthesis: step complete")
	}

	log.Info().Str("recipe", r.Name).Msg("synthesis: pipeline complete")
	onProgress(ProgressEvent{Phase: "done"})
	return nil
}
