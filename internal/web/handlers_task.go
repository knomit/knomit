// Async task handlers for long-running operations (synthesis, rebuild, git sync).
// Tasks run in the background via TaskHub; clients poll via SSE.
package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"
	"knomit/internal/repos"
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
		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		deps, hub, repo := ri.SynthDeps, ri.Hub, ri.Name
		ri.RUnlock()

		if deps == nil || deps.Adapter == nil || deps.Reviewer == nil {
			log.Warn().Msg("synthesize: not available (no LLM configured)")
			writeError(w, http.StatusServiceUnavailable, "synthesis not available")
			return
		}

		log.Info().Msg("synthesize: starting review")

		id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
			emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "review starting", Repo: repo})
			if err := deps.Reviewer.RunAll(ctx, deps.Adapter); err != nil {
				emit(repos.TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(repos.TaskEvent{Status: "done", Message: "review complete", Repo: repo})
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
		ri := repos.RepoFromContext(r.Context())
		ri.RLock()
		svc, gs, hub, repo, agentBranch := ri.Svc, ri.GS, ri.Hub, ri.Name, ri.AgentBranch
		ri.RUnlock()

		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "index not available")
			return
		}

		gitReader, ok := gs.(store.GitReader)
		if !ok {
			writeError(w, http.StatusInternalServerError, "git store does not support rebuild")
			return
		}

		idx := svc.Index()
		branch := agentBranch

		id, err := hub.Start("rebuild", func(ctx context.Context, emit func(repos.TaskEvent)) {
			emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "rebuilding index", Repo: repo})
			progress := func(subPhase string, done, total int) {
				if done%10 == 0 || done == total {
					emit(repos.TaskEvent{Status: "running", Phase: subPhase, Message: fmt.Sprintf("%d/%d", done, total), Repo: repo})
				}
			}
			if err := idx.Rebuild(gitReader, branch, progress); err != nil {
				emit(repos.TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(repos.TaskEvent{Status: "done", Message: "rebuild complete", Repo: repo})
		})
		if err != nil {
			writeTaskConflict(w, "rebuild", err)
			return
		}

		writeTaskStarted(w, "rebuild", id)
	}
}
