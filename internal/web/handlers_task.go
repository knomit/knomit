// Async task handlers for long-running operations (synthesis, rebuild, git sync).
// Tasks run in the background via TaskHub; clients poll via SSE.
package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"
	"knomit/internal/store"
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
// Runs the Reviewer (multi-turn review session) in the background via TaskHub.
func handleSynthesizeStart() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := RepoFromContext(r.Context())
		deps := ri.SynthDeps
		if deps == nil || deps.Adapter == nil || deps.Reviewer == nil {
			log.Warn().Msg("synthesize: not available (no LLM configured)")
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}

		log.Info().Msg("synthesize: starting review")

		repo := ri.Name
		id, err := ri.Hub.Start("synth", func(ctx context.Context, emit func(TaskEvent)) {
			emit(TaskEvent{Status: "running", Phase: "start", Message: "review starting", Repo: repo})
			if err := deps.Reviewer.RunAll(ctx, deps.Adapter); err != nil {
				emit(TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(TaskEvent{Status: "done", Message: "review complete", Repo: repo})
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
			progress := func(phase string, done, total int) {
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


