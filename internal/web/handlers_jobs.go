// Async job POST handlers for branch-scoped long-running operations.
// Clients start jobs via POST and observe progress through the branch events
// SSE stream at /repos/{repo}/branches/{branch}/events.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
	"knomit/internal/web/hal"
)

// jobEnvelope is the response body returned by job POST handlers.
type jobEnvelope struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	State     string       `json:"state"`
	CreatedAt time.Time    `json:"created_at,omitempty"`
	UpdatedAt time.Time    `json:"updated_at,omitempty"`
	Progress  *JobProgress `json:"progress,omitempty"`
	Error     string       `json:"error,omitempty"`
	Links     hal.LinkMap  `json:"_links,omitempty"`
}

// jobEnvelopeFromEntry converts a registry entry to a jobEnvelope.
func jobEnvelopeFromEntry(e *JobEntry) jobEnvelope {
	return jobEnvelope{
		ID:        e.ID,
		Kind:      e.Kind,
		State:     e.State,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
		Progress:  e.Progress,
		Error:     e.Error,
	}
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
			Kind:  "synthesis-run",
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
			th := newProgressThrottle(250 * time.Millisecond)
			progress := func(subPhase string, done, total int) {
				if th.allow(done, total) {
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
			Kind:  "index-rebuild",
			State: "running",
		})
	}
}

// handleListJobs serves GET .../synthesis-runs or .../index-rebuilds.
func handleListJobs(jr *JobRegistry, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if jr == nil {
			hal.WriteHAL(w, http.StatusOK, hal.CollectionView[jobEnvelope]{
				Count:    0,
				Links:    hal.LinkMap{"self": {Href: r.URL.Path}},
				Embedded: map[string][]jobEnvelope{"jobs": {}},
			})
			return
		}
		entries := jr.List(kind)
		items := make([]jobEnvelope, 0, len(entries))
		for _, e := range entries {
			items = append(items, jobEnvelopeFromEntry(e))
		}
		view := hal.CollectionView[jobEnvelope]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: r.URL.Path}},
			Embedded: map[string][]jobEnvelope{"jobs": items},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleGetJob serves GET .../synthesis-runs/{id} or .../index-rebuilds/{id}.
func handleGetJob(jr *JobRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if jr == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Job not found", "job "+id+" not found", r.URL.Path)
			return
		}
		e := jr.Get(id)
		if e == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Job not found", "job "+id+" not found", r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, jobEnvelopeFromEntry(e))
	}
}

// handleDeleteJob serves DELETE .../synthesis-runs/{id} or .../index-rebuilds/{id}.
// If the job is still running, returns 409 Conflict (cancel not supported via TaskHub).
func handleDeleteJob(jr *JobRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if jr == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		e := jr.Get(id)
		if e == nil {
			w.WriteHeader(http.StatusNoContent) // idempotent
			return
		}
		if e.State == "running" {
			hal.WriteProblem(w, http.StatusConflict, "Job is running",
				"job "+id+" is still running; cancellation is not supported", r.URL.Path)
			return
		}
		jr.Delete(id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleJobEvents serves GET .../synthesis-runs/{id}/events or .../index-rebuilds/{id}/events.
// Streams SSE events from the TaskHub filtered to the given job ID.
func handleJobEvents(m *repos.Manager, jr *JobRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		id := chi.URLParam(r, "id")

		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		// If no registry, return 404 for unknown jobs.
		if jr != nil {
			if e := jr.Get(id); e == nil {
				hal.WriteProblem(w, http.StatusNotFound, "Job not found",
					"job "+id+" not found", r.URL.Path)
				return
			}
		}

		hub := ri.TaskHub()
		if hub == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Task hub unavailable",
				"task hub not initialised for this repo", r.URL.Path)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		events, snapshot := hub.Subscribe(r.Context())

		// Replay snapshot events matching this job ID.
		for _, ev := range snapshot {
			if ev.ID == id {
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
			}
		}
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				if ev, isTask := e.(repos.TaskEvent); isTask && ev.ID == id {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "event: task\ndata: %s\n\n", data)
					flusher.Flush()
					if ev.Status == "done" || ev.Status == "error" {
						return
					}
				}
			}
		}
	}
}
