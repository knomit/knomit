// Async job POST handlers for branch-scoped long-running operations.
// Clients start jobs via POST and observe progress through the branch events
// SSE stream at /repos/{repo}/branches/{branch}/events.
package web

import (
	"context"
	"encoding/json"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"time"

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

// handleDeleteJob serves DELETE .../synthesis-runs/{id} or .../index-rebuilds/{id}:
// it CANCELS the job. There is no queue to take a job out of — a synthesis
// start while one runs is 409, a rebuild replaces or absorbs the running one
// — so the only job DELETE can act on is the kind's running one:
//   - synthesis-run: TaskHub.Cancel cancels the task's context. 202 with
//     the job, still running. The run stops at its next model call and
//     usually ends in error with a cancellation message; a run that was
//     already finishing (its last item in flight) may end done.
//   - index-rebuild: the lifecycle machine owns the rebuild, so this is the
//     same CancelIndex that POST /repos/{repo}/index:cancel sends; it drains,
//     and the job ends in error "indexing cancelled". 202 with the job.
//
// A finished job is 409 "Job already finished", an unknown or other-kind id
// 404. DELETE never answers success without cancelling the job it names.
func handleDeleteJob(b hal.URLBuilder, m *repos.Manager, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		ri := repos.RepoFromContext(r.Context())
		ev, ok := hubTask(ri, id, kind)
		if !ok {
			writeJobNotFound(w, r, id)
			return
		}
		if terminalTask(ev.Status) {
			writeJobFinished(w, r, id)
			return
		}
		switch kind {
		case "synthesis-run":
			running, cancelled := ri.TaskHub().Cancel(id)
			if !cancelled { // it finished between the lookup and the cancel
				writeJobFinished(w, r, id)
				return
			}
			hal.WriteHAL(w, http.StatusAccepted, jobOfTask(running, kind))
		default:
			// CancelIndex cancels whatever index job runs, and the lookup
			// above said it is this rebuild. The reply waits for the drain,
			// so the hub then holds the rebuild's terminal: "indexing
			// cancelled" when this cancel ended it; anything else means it
			// ended on its own first.
			if _, err := m.Send(r.Context(), ri, repos.CancelIndex()); err != nil {
				httpx.WriteLifecycleProblem(w, r, b, chi.URLParam(r, "repo"), err)
				return
			}
			after, held := hubTask(ri, id, kind)
			if !held || (terminalTask(after.Status) && after.Message != repos.ErrIndexCancelled.Error()) {
				writeJobFinished(w, r, id)
				return
			}
			hal.WriteHAL(w, http.StatusAccepted, jobOfTask(after, kind))
		}
	}
}

func writeJobFinished(w http.ResponseWriter, r *http.Request, id string) {
	hal.WriteProblem(w, http.StatusConflict, "Job already finished",
		"job "+id+" has already finished; there is nothing to cancel", r.URL.Path)
}

// jobEventsKeepalive is how long a job stream may sit idle before it writes
// the shared `: keepalive` comment frame. A var so a test can shorten it.
var jobEventsKeepalive = 30 * time.Second

// handleJobEvents serves GET .../synthesis-runs/{id}/events or .../index-rebuilds/{id}/events.
//
// It subscribes FIRST and only then reads the job from the hub. The hub
// records a terminal before it publishes it (TaskHub.Start's emit and
// publishTask), so the read either already sees the terminal — the frame is
// written and the stream ends — or sees the job running, and then the
// terminal is published after the subscription and arrives on it. One
// mechanism, no window. An id the hub does not hold is 404 before any stream
// opens; a running frame can repeat once (read, then delivered), which is
// harmless.
func handleJobEvents(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		ri := repos.RepoFromContext(r.Context())
		if ri == nil || ri.TaskHub() == nil {
			writeJobNotFound(w, r, id)
			return
		}
		events, _ := ri.TaskHub().Subscribe(r.Context())
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
		if !write(ev) || terminalTask(ev.Status) {
			return
		}

		// A synthesis waiting on the model can emit nothing for minutes;
		// keepalive stops a proxy or browser dropping the idle stream.
		keepalive := time.NewTicker(jobEventsKeepalive)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				if !stream.Keepalive() {
					return
				}
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
