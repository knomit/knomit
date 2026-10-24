package origins

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/platform/fileuri"
)

// testServer is what this package's tests drive instead of web's Server: the
// routes Mount registers, over a session manager the test holds, mounted at
// the path and behind the repo lookup web's router gives them
// (/repos/{repo}/origin-sessions inside RepoMiddleware). No auth edge runs:
// web's router tests cover that, and these test the handlers.
type testServer struct {
	Manager *repos.Manager
	h       http.Handler
}

func newTestServer(m *repos.Manager, agentBranch string, sm *sessionManager) *testServer {
	r := chi.NewRouter()
	r.Route("/repos/{repo}", func(r chi.Router) {
		r.Use(repoFromURL(m))
		r.Route("/origin-sessions", func(r chi.Router) { routes(r, m, agentBranch, sm) })
	})
	return &testServer{Manager: m, h: r}
}

func (s *testServer) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.h.ServeHTTP(w, r) }

// repoFromURL is the part of web's RepoMiddleware these handlers depend on:
// the {repo} instance in the request context, 404 when there is none.
func repoFromURL(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ri := m.Get(chi.URLParam(r, "repo"))
			if ri == nil {
				hal.WriteProblem(w, http.StatusNotFound, "Repo not found", "no repo", r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
		})
	}
}

// newTestSessionManager is a session manager over a root of t's own whose
// lifetime ends when t does (close at t.Cleanup), so a test's sessions and
// their folders go with it.
func newTestSessionManager(t *testing.T) *sessionManager {
	t.Helper()
	sm := newSessionManager(context.Background(), t.TempDir())
	t.Cleanup(sm.close)
	return sm
}

// newJSONRequest is httptest.NewRequest with the JSON Content-Type
// (a copy of web's fixture of the same name).
func newJSONRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

// streamRecorder is the part of web's shared SSE test double these tests use:
// a locked recorder that satisfies http.ResponseController (Flush and
// SetWriteDeadline) and can be told to fail its writes.
type streamRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	writeErr  error
	deadlines int
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (s *streamRecorder) SetWriteDeadline(time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines++
	return nil
}

func (s *streamRecorder) failWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErr = err
}

func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.ResponseRecorder.Write(b)
}

func (s *streamRecorder) Flush() {}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Body.String()
}

// seedBareKBOn builds a bare knowledge-base remote (the default ontology, one
// commit) whose only branch, and HEAD, is branch (a copy of web's fixture).
func seedBareKBOn(t *testing.T, bare, branch string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch="+branch, bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	runGitForTest(t, work, "checkout", "-B", branch)
	ont, err := fact.DefaultOntology().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, filepath.Dir(repos.OntologyPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, repos.OntologyPath), ont, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-m", "seed kb")
	runGitForTest(t, work, "push", "origin", branch)
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return fileuri.New(bare)
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
