// Async job POST handlers for branch-scoped long-running operations.
// Clients start jobs via POST and observe progress through the branch events
// SSE stream at /repos/{repo}/branches/{branch}/events.
package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
	"knomit/internal/web/hal"
)

// jobEnvelope is the response body returned by job POST handlers.
type jobEnvelope struct {
	ID    string      `json:"id"`
	Kind  string      `json:"kind"`
	State string      `json:"state"`
	Links hal.LinkMap `json:"_links"`
}

// handleStartSynthesis handles POST /api/v1/repos/{repo}/branches/{branch}/synthesis-runs.
// Starts a synthesis review job in the background via TaskHub and returns a
// job envelope. Returns 503 if no LLM adapter is configured, 409 if a
// synthesis job is already running.
func handleStartSynthesis(m *repos.Manager, llmAdapter llm.LLMAdapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if llmAdapter == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Synthesis unavailable",
				"no LLM adapter configured", r.URL.Path)
			return
		}

		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		hub := ri.TaskHub()
		if hub == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Task hub unavailable",
				"task hub not initialised for this repo", r.URL.Path)
			return
		}

		reviewer := synthesize.NewReviewer(ri, nil)
		repo := ri.Name()

		id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
			emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "review starting", Repo: repo})
			if err := reviewer.RunAll(ctx, llmAdapter); err != nil {
				emit(repos.TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(repos.TaskEvent{Status: "done", Message: "review complete", Repo: repo})
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusConflict, "Job already running",
				err.Error(), r.URL.Path)
			return
		}

		hal.WriteHAL(w, http.StatusCreated, jobEnvelope{
			ID:    id,
			Kind:  "synthesis",
			State: "running",
		})
	}
}

// handleStartRebuild handles POST /api/v1/repos/{repo}/branches/{branch}/index-rebuilds.
// Clears the index last-commit marker and re-indexes every file from HEAD,
// emitting progress events via TaskHub. Returns 409 if a rebuild is already running.
func handleStartRebuild(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())

		var svc *store.Service
		ri.WithRead(func(s *store.Service) { svc = s })
		if svc == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Index unavailable",
				"store not yet available for this repo", r.URL.Path)
			return
		}

		hub := ri.TaskHub()
		if hub == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Task hub unavailable",
				"task hub not initialised for this repo", r.URL.Path)
			return
		}

		repo := ri.Name()

		id, err := hub.Start("rebuild", func(ctx context.Context, emit func(repos.TaskEvent)) {
			emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "rebuilding index", Repo: repo})
			progress := func(subPhase string, done, total int) {
				if done%10 == 0 || done == total {
					emit(repos.TaskEvent{Status: "running", Phase: subPhase, Message: fmt.Sprintf("%d/%d", done, total), Repo: repo})
				}
			}
			if err := svc.IndexManager().Rebuild(ctx, branch, progress); err != nil {
				emit(repos.TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(repos.TaskEvent{Status: "done", Message: "rebuild complete", Repo: repo})
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusConflict, "Job already running",
				err.Error(), r.URL.Path)
			return
		}

		hal.WriteHAL(w, http.StatusCreated, jobEnvelope{
			ID:    id,
			Kind:  "rebuild",
			State: "running",
		})
	}
}
