// Async task handlers for long-running operations (synthesis, rebuild, git sync).
// Tasks run in the background via TaskHub; clients poll via SSE.
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// writeTaskStarted writes a 200 response for a successfully started task.
func writeTaskStarted(w http.ResponseWriter, op, id string) {
	writeJSON(w, http.StatusOK, map[string]any{"op": op, "id": id, "status": "running"})
}

// writeTaskConflict writes a 409 response when a task is already running.
func writeTaskConflict(w http.ResponseWriter, op string, err error) {
	writeJSON(w, http.StatusConflict, map[string]any{"op": op, "status": "error", "message": err.Error()})
}

// handleSynthesizeStart handles POST /api/v1/{repo}/synthesize.
// The recipe is validated synchronously so that a malformed recipe
// produces a 400 immediately rather than an async error. If the recipe
// is valid, execution proceeds in the background via TaskHub.
func handleSynthesizeStart() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		deps := ri.SynthDeps
		if deps == nil || deps.Adapter == nil {
			log.Warn().Msg("synthesize: not available (no LLM configured)")
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read body error: %v", err))
			return
		}

		// Parse recipe before starting task — bad recipe gets a 400, not an async error.
		recipeYAML := string(body)
		if recipeYAML == "" {
			recipeYAML = defaultRecipe
		}
		recipe, err := synthesize.ParseRecipe(recipeYAML)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipe: %v", err))
			return
		}

		log.Info().Str("recipe", recipe.Name).Msg("synthesize: starting")

		var emb synthesize.Embedder
		if deps.Embedder != nil {
			emb = deps.Embedder
		}

		repo := ri.Name
		id, err := ri.Hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Phase: "start", Message: "synthesis starting", Repo: repo})
			onProgress := func(ev synthesize.ProgressEvent) {
				emit(TaskEvent{Status: "running", Phase: ev.Phase, Message: ev.Message, Repo: repo})
			}
			if err := synthesize.Run(ctx, deps.GS, deps.Idx, emb, deps.Adapter, recipe, onProgress); err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(TaskEvent{Status: "done", Message: "synthesis complete", Repo: repo})
		})
		if err != nil {
			writeTaskConflict(w, "synth", err)
			return
		}

		writeTaskStarted(w, "synth", id)
	}
}

// handleRebuild handles POST /api/v1/{repo}/rebuild.
// Clears the index last-commit marker and re-indexes every file from HEAD,
// emitting progress events via TaskHub.
func handleRebuild() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		if ri.Svc == nil {
			writeError(w, http.StatusServiceUnavailable, "index not available")
			return
		}

		gitReader, ok := ri.GS.(store.GitReader)
		if !ok {
			writeError(w, http.StatusInternalServerError, "git store does not support rebuild")
			return
		}

		idx := ri.Svc.Index()
		branch := ri.GS.Branch()

		repo := ri.Name
		id, err := ri.Hub.Start("rebuild", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Phase: "start", Message: "rebuilding index", Repo: repo})
			progress := func(done, total int) {
				if done%10 == 0 || done == total {
					emit(TaskEvent{Status: "running", Phase: "indexing", Message: fmt.Sprintf("%d/%d files", done, total), Repo: repo})
				}
			}
			if err := idx.Rebuild(gitReader, branch, progress); err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(TaskEvent{Status: "done", Message: "rebuild complete", Repo: repo})
		})
		if err != nil {
			writeTaskConflict(w, "rebuild", err)
			return
		}

		writeTaskStarted(w, "rebuild", id)
	}
}

// defaultRecipe is used when the POST body is empty, providing a
// sensible default synthesis operation (prune then distill).
const defaultRecipe = `name: default
prompt: Review and consolidate the knowledge base.
steps:
  - mode: prune
    prompt: Identify stale, redundant, or outdated facts.
  - mode: distill
    prompt: Find patterns across facts and create higher-level summaries.
`

