// Async job POST handlers for branch-scoped long-running operations.
// Clients start jobs via POST and observe progress through the branch events
// SSE stream at /repos/{repo}/branches/{branch}/events.
package web

import (
	"context"
	"encoding/json"
	"knomit/internal/edge/web/httpx"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/synthesize"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/llm"
)

// jobEnvelope is the response body of every job route: a job as the repo's
// TaskHub holds it. The hub is the only record of a job; it keeps an op's
// running task and its last terminal one, nothing older, and no timestamps
// or progress counters (progress is on the branch events stream).
type jobEnvelope struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"` // the task status: running, done or error
	Error string `json:"error,omitempty"`
}

// handleStartSynthesis handles POST /api/v1/repos/{repo}/branches/{branch}/synthesis-runs.
// Starts a synthesis review job in the background via TaskHub and returns a
// job envelope. Returns 503 if no LLM adapter is configured, 409 if a
// synthesis job is already running.
func handleStartSynthesis(llmAdapter llm.LLMAdapter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if llmAdapter == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Synthesis unavailable",
				"no LLM adapter configured", r.URL.Path)
			return
		}

		ri := repos.RepoFromContext(r.Context())
		// The reviewer writes on the repo's agent branch whatever branch the
		// URL names, so that is the branch that must accept authored writes
		// (the fleet repository and a subscription accept none).
		if httpx.RefuseUnwritableBranch(w, r, ri, ri.AgentBranch()) {
			return
		}

		hub := ri.TaskHub()
		if hub == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Task hub unavailable",
				"task hub not initialised for this repo", r.URL.Path)
			return
		}

		// Pin the store for the task's lifetime: the Reviewer resolves store
		// indices from ri per operation and the task outlives this request, so
		// without the pin an Archive/SwapStore could close the SQLite handle
		// mid-review. Teardown cancels hub tasks first (shutdown → hub.Shutdown
		// precedes closeFn), so the drain does not wait on a task that will
		// never be told to stop.
		unpin, err := ri.Pin()
		if err != nil {
			httpx.WriteStoreUnavailable(w, r)
			return
		}

		reviewer := synthesize.NewReviewer(ri, nil)
		repo := ri.Name()

		id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
			defer unpin()
			emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "review starting", Repo: repo})
			if err := reviewer.RunAll(ctx, llmAdapter); err != nil {
				emit(repos.TaskEvent{Status: "error", Message: err.Error(), Repo: repo})
				return
			}
			emit(repos.TaskEvent{Status: "done", Message: "review complete", Repo: repo})
		})
		if err != nil {
			unpin()
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
//
// It sends Rebuild(branch) to the repo's lifecycle machine, which restarts its
// Index stage as a full rebuild of the branch. A running index job — the heal,
// or a rebuild of another branch — is cancelled and REPLACED (201); an
// identical rebuild already running is ABSORBED (200, the running job's
// envelope). There is no 409: there is exactly one Index life, and no status
// cell to contend for. Sync and Serve are untouched. Progress is on the
// branch events stream (task events on op "rebuild") and in the repo's
// index_state.
func handleStartRebuild(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := httpx.BranchFromContext(r.Context())
		rep, err := m.Send(r.Context(), ri, repos.Rebuild(branch))
		if err != nil {
			httpx.WriteLifecycleProblem(w, r, b, chi.URLParam(r, "repo"), err)
			return
		}
		status := http.StatusCreated
		if rep.Absorbed {
			status = http.StatusOK
		}
		hal.WriteHAL(w, status, jobEnvelope{
			ID:    rep.JobID,
			Kind:  "index-rebuild",
			State: "running",
		})
	}
}

// hubOps is the TaskHub op of each job kind.
var hubOps = map[string]string{"index-rebuild": "rebuild", "synthesis-run": "synth"}

// jobOfTask is the envelope of task ev as a job of kind.
func jobOfTask(ev repos.TaskEvent, kind string) jobEnvelope {
	env := jobEnvelope{ID: ev.ID, Kind: kind, State: ev.Status}
	if ev.Status == "error" {
		env.Error = ev.Message
	}
	return env
}

// terminalTask reports whether a task status is final.
func terminalTask(status string) bool { return status == "done" || status == "error" }

// hubTask is job id of kind as ri's TaskHub holds it: ok is false for an id
// the hub does not hold (never seen, or older than the op's last terminal
// task) and for an id of the other kind's op.
func hubTask(ri *repos.RepoInstance, id, kind string) (repos.TaskEvent, bool) {
	if ri == nil || ri.TaskHub() == nil {
		return repos.TaskEvent{}, false
	}
	ev, ok := ri.TaskHub().Task(id)
	if !ok || ev.Op != hubOps[kind] {
		return repos.TaskEvent{}, false
	}
	return ev, true
}

func writeJobNotFound(w http.ResponseWriter, r *http.Request, id string) {
	hal.WriteProblem(w, http.StatusNotFound, "Job not found", "job "+id+" not found", r.URL.Path)
}

// handleListJobs serves GET .../synthesis-runs or .../index-rebuilds: the
// jobs of kind the repo's TaskHub holds - its running job first, then its
// last finished one; at most two, none before the first run.
func handleListJobs(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items := []jobEnvelope{}
		if ri := repos.RepoFromContext(r.Context()); ri != nil && ri.TaskHub() != nil {
			for _, ev := range ri.TaskHub().Tasks(hubOps[kind]) {
				items = append(items, jobOfTask(ev, kind))
			}
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[jobEnvelope]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: r.URL.Path}},
			Embedded: map[string][]jobEnvelope{"jobs": items},
		})
	}
}

// handleGetJob serves GET .../synthesis-runs/{id} or .../index-rebuilds/{id}
// from the repo's TaskHub, where both the synthesis task and the lifecycle
// machine's rebuild (publishRebuildTask) are recorded; the latter is the job
// POST /fleet/index-rebuild links (#529 S1). The hub holds an op's running
// task and its last terminal one, so an older job is 404.
func handleGetJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		ev, ok := hubTask(repos.RepoFromContext(r.Context()), id, kind)
		if !ok {
			writeJobNotFound(w, r, id)
			return
		}
		hal.WriteHAL(w, http.StatusOK, jobOfTask(ev, kind))
	}
}

// handleDeleteJob serves DELETE .../synthesis-runs/{id} or .../index-rebuilds/{id}.
// Nothing is ever deleted: the TaskHub has no cancel by job id and no way to
// forget a finished task (it drops one only when the op's next task
// finishes). An unknown job is 404; a running one 409 "Job is running"; a
// finished one 409 "Job cannot be deleted".
func handleDeleteJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		ev, ok := hubTask(repos.RepoFromContext(r.Context()), id, kind)
		if !ok {
			writeJobNotFound(w, r, id)
			return
		}
		if !terminalTask(ev.Status) {
			hal.WriteProblem(w, http.StatusConflict, "Job is running",
				"job "+id+" is still running; cancellation is not supported", r.URL.Path)
			return
		}
		hal.WriteProblem(w, http.StatusConflict, "Job cannot be deleted",
			"job "+id+" has finished; the server keeps the last finished job until the next one finishes", r.URL.Path)
	}
}

// handleJobEvents serves GET .../synthesis-runs/{id}/events or .../index-rebuilds/{id}/events.
// An id the TaskHub does not hold is 404 before any stream opens. Otherwise
// the stream carries the job's task events and ENDS with its terminal one,
// at once for a job that has already finished.
func handleJobEvents(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		ri := repos.RepoFromContext(r.Context())
		ev, ok := hubTask(ri, id, kind)
		if !ok {
			writeJobNotFound(w, r, id)
			return
		}

		stream, ok := httpx.StartSSE(w)
		if !ok {
			return
		}
		write := func(ev repos.TaskEvent) bool {
			data, _ := json.Marshal(ev)
			return stream.Write("event: task\ndata: %s\n\n", data)
		}
		if terminalTask(ev.Status) {
			write(ev)
			return
		}

		events, snapshot := ri.TaskHub().Subscribe(r.Context())
		found := false
		for _, s := range snapshot {
			if s.ID != id {
				continue
			}
			found = true
			if !write(s) || terminalTask(s.Status) {
				return
			}
		}
		if !found {
			// The job finished between the lookup and the subscription and
			// the snapshot no longer shows it (a newer task of the op runs):
			// its terminal event, while the hub still holds it, ends the
			// stream.
			if last, held := hubTask(ri, id, kind); held {
				write(last)
			}
			return
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				if ev, isTask := e.(repos.TaskEvent); isTask && ev.ID == id {
					if !write(ev) {
						return
					}
					// This stream is for ONE job, so its terminal event is
					// the end of the stream, not just of the event.
					if terminalTask(ev.Status) {
						return
					}
				}
			}
		}
	}
}
