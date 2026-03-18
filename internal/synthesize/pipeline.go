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
	WriteFile(path, content, message, operation string) (commitHash, blobHash string, err error)
	BatchWrite(files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFile(path, message, operation string) (commitHash string, err error)
	ListAll() ([]string, error)
	Tag(name string) error
	Branch() string
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit() (string, error)
}

// SearchIndex is the interface that the synthesize pipeline requires from the search index.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	Upsert(r store.FactRecord) error
	Delete(path string) error
	ClusterFacts(resolution float64, minCommunitySize int) (store.ClusterResult, error)
	GraphAddDerivedFrom(newPath string, sourcePaths []string) error
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

// namedProfiles maps explicit profile names to their canonical Profile struct.
var namedProfiles = map[string]Profile{
	"small": SmallProfile,
	"large": LargeProfile,
}

// resolveStepProfile returns the profile for a step, using the step's explicit
// override or auto-detecting from the adapter's model name.
func resolveStepProfile(step RecipeStep, adapter llm.LLMAdapter) Profile {
	var p Profile
	if named, ok := namedProfiles[step.Profile]; ok {
		p = named
	} else {
		p = ResolveProfile(adapter.Model())
	}
	if step.RetryOnPassive != nil {
		p.RetryOnPassive = *step.RetryOnPassive
	}
	return p
}

// Run executes a synthesis recipe against the provided git store and search index.
// onProgress is called for each pipeline event; if nil, a no-op is used.
func Run(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, r Recipe, onProgress func(ProgressEvent)) error {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}

	log.Info().Str("recipe", r.Name).Int("steps", len(r.Steps)).Msg("synthesis: pipeline starting")

	for i, step := range r.Steps {
		profile := resolveStepProfile(step, adapter)
		log.Info().Str("mode", step.Mode).Str("profile", profile.Name).Int("step", i+1).Int("total", len(r.Steps)).Msg("synthesis: step starting")
		onProgress(ProgressEvent{Phase: "step-start", Message: step.Mode})
		var err error
		switch step.Mode {
		case "prune":
			err = executePruneStep(ctx, gs, idx, embedder, adapter, step, r, profile, onProgress)
		case "distill":
			err = executeDistillStep(ctx, gs, idx, embedder, adapter, step, r, profile, onProgress)
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
