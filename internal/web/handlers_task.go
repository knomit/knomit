// Async task handlers for long-running operations (synthesis, git sync).
// Tasks run in the background via TaskHub; clients poll via SSE.
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
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

// handleSynthesizeStart handles POST /api/v1/synthesize.
// The recipe is validated synchronously so that a malformed recipe
// produces a 400 immediately rather than an async error. If the recipe
// is valid, execution proceeds in the background via TaskHub.
func handleSynthesizeStart(deps *SynthDeps, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		id, err := hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Phase: "start", Message: "synthesis starting"})
			onProgress := func(ev synthesize.ProgressEvent) {
				emit(TaskEvent{Status: "running", Phase: ev.Phase, Message: ev.Message})
			}
			if err := synthesize.Run(ctx, deps.GS, deps.Idx, emb, deps.Adapter, recipe, onProgress); err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error()})
				return
			}
			emit(TaskEvent{Status: "done", Message: "synthesis complete"})
		})
		if err != nil {
			writeTaskConflict(w, "synth", err)
			return
		}

		writeTaskStarted(w, "synth", id)
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

// handleSync handles POST /api/v1/sync
// TODO: re-wire to trigger the background sync goroutine instead of calling gs.Sync directly.
func handleSync(gs GitStore, hub *TaskHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "sync moved to background goroutine")
	}
}
