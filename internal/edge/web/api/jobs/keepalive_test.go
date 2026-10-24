package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// testRouter is what this package's tests drive instead of web's Server: the
// routes Mount registers at /repos/{repo}/branches/{branch}, behind the repo
// lookup and branch parse web's router gives them. No auth edge runs: the job
// routes' contract tests run through web's router.
func testRouter(m *repos.Manager) http.Handler {
	r := chi.NewRouter()
	r.Route("/repos/{repo}", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ri := m.Get(chi.URLParam(r, "repo"))
				if ri == nil {
					hal.WriteProblem(w, http.StatusNotFound, "Repo not found", "no repo", r.URL.Path)
					return
				}
				next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
			})
		})
		r.Route("/branches/{branch}", func(r chi.Router) {
			r.Use(httpx.BranchMiddleware)
			Mount(r, m, nil)
		})
	})
	return r
}

// streamRecorder is the part of web's shared SSE test double this test uses:
// a locked recorder that satisfies http.ResponseController (Flush and
// SetWriteDeadline).
type streamRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func (s *streamRecorder) SetWriteDeadline(time.Time) error { return nil }
func (s *streamRecorder) Flush()                           {}

func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Write(b)
}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Body.String()
}

// An IDLE job stream writes the shared keepalive comment frame, so a proxy
// does not drop a job that is quiet while it waits on the model.
//
// It sets the package var jobEventsKeepalive, so tests here must not run in
// parallel.
func TestJobs_EventsKeepaliveOnAnIdleStream(t *testing.T) {
	old := jobEventsKeepalive
	jobEventsKeepalive = 20 * time.Millisecond
	t.Cleanup(func() { jobEventsKeepalive = old })

	hub := repos.NewTaskHub(context.Background())
	t.Cleanup(hub.Shutdown)
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", AgentBranch: "agent/test", Hub: hub,
	}))

	gate := make(chan struct{})
	started := make(chan struct{})
	id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
		emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "review starting"})
		close(started)
		select {
		case <-gate:
		case <-ctx.Done():
		}
	})
	if err != nil {
		t.Fatalf("start held synth: %v", err)
	}
	<-started
	t.Cleanup(func() { close(gate) })

	ctx, cancel := context.WithCancel(context.Background())
	rec := &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testRouter(m).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/repos/alpha/branches/agent:test/synthesis-runs/"+id+"/events", nil).WithContext(ctx))
	}()
	defer func() { cancel(); <-done }()

	// The job emits nothing more; the only thing that can arrive is a keepalive.
	deadline := time.After(3 * time.Second)
	for !strings.Contains(rec.body(), ": keepalive\n\n") {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for a keepalive frame; body so far:\n%s", rec.body())
		case <-time.After(10 * time.Millisecond):
		}
	}
	var frames []repos.TaskEvent
	for _, frame := range strings.Split(rec.body(), "\n\n") {
		if data, ok := strings.CutPrefix(frame, "event: task\ndata: "); ok {
			var ev repos.TaskEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("task frame is not JSON: %v (%q)", err, data)
			}
			frames = append(frames, ev)
		}
	}
	if len(frames) != 1 || frames[0].Status != "running" {
		t.Fatalf("frames = %+v, want only the initial running frame", frames)
	}
}
