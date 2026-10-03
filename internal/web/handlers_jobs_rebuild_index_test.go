package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// newIndexTestServer brings up a real Manager with one created repo,
// returning the router and the repo's instance.
func newIndexTestServer(t *testing.T, repo string) (http.Handler, *repos.RepoInstance) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createViaAPI(t, r, repo)

	ri := m.Get(repo)
	if ri == nil {
		t.Fatalf("repo %s missing after create", repo)
	}
	// createViaAPI waits for the create job, which reports done only once the
	// index has left "indexing", so every test below starts terminal.
	if state := ri.Status().Index.State; state != "ready" {
		t.Fatalf("repo %s is %q after create, want ready", repo, state)
	}
	return r, ri
}

func postRebuildOn(t *testing.T, r http.Handler, repo, branch string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodPost,
		"/repos/"+repo+"/branches/"+branch+"/index-rebuilds", nil)))
	return rec
}

func postRebuild(t *testing.T, r http.Handler, repo string) *httptest.ResponseRecorder {
	t.Helper()
	return postRebuildOn(t, r, repo, "machine:test")
}

func jobOf(t *testing.T, rec *httptest.ResponseRecorder) jobEnvelope {
	t.Helper()
	var env jobEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("job envelope is not JSON: %v (%s)", err, rec.Body.String())
	}
	return env
}

// A MANUAL REBUILD IS AN INDEX JOB: it restarts the Index stage as a full
// rebuild, so the derived status, the REST payload and the event stream all
// read "indexing" while it runs and its terminal once it ends.
func TestRebuild_MarksIndexingThenReady(t *testing.T) {
	r, ri := newIndexTestServer(t, "alpha")

	rec, stop := openIndexStream(t, r)
	defer stop()

	resp := postRebuild(t, r, "alpha")
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST rebuild: got %d, want 201\nbody: %s", resp.Code, resp.Body.String())
	}
	if env := jobOf(t, resp); env.ID == "" || env.Kind != "index-rebuild" || env.State != "running" {
		t.Fatalf("job envelope = %+v", env)
	}

	body := rec.waitFor(t, "a terminal index frame", func(b string) bool {
		for _, e := range indexFrames(t, b) {
			if e.Repo == "alpha" && (e.State == "ready" || e.State == "error") {
				return true
			}
		}
		return false
	})

	var mine []repos.IndexEvent
	for _, e := range indexFrames(t, body) {
		if e.Repo == "alpha" {
			mine = append(mine, e)
		}
	}
	got := states(mine)
	if len(got) < 2 || got[0] != "indexing" || got[len(got)-1] != "ready" {
		t.Fatalf("state sequence = %v, want indexing … ready\nbody:\n%s", got, body)
	}

	// The REST view agrees once it is over.
	if state := ri.Status().Index.State; state != "ready" {
		t.Fatalf("index state after rebuild = %q, want ready", state)
	}
}

// REPLACE OR ABSORB, NEVER 409. While an index job runs, an identical rebuild
// is absorbed — 200 with the running job's envelope — and a rebuild of another
// branch replaces the running job — 201 with a new one. There is exactly one
// Index life and no status cell to contend for, so neither is refused.
func TestRebuild_AbsorbsAnIdenticalJobAndReplacesAnother(t *testing.T) {
	r, _, release := heldIndexServer(t) // a rebuild of machine/test is running, held
	first := postRebuild(t, r, "alpha")
	if first.Code != http.StatusOK {
		t.Fatalf("identical rebuild while one runs: got %d, want 200 (absorbed)\nbody: %s", first.Code, first.Body.String())
	}
	absorbed := jobOf(t, first)

	again := postRebuild(t, r, "alpha")
	if again.Code != http.StatusOK || jobOf(t, again).ID != absorbed.ID {
		t.Fatalf("a second identical rebuild must be absorbed into the same job: %d %s", again.Code, again.Body.String())
	}

	other := postRebuildOn(t, r, "alpha", "main")
	if other.Code != http.StatusCreated {
		t.Fatalf("rebuild of another branch: got %d, want 201 (replaces)\nbody: %s", other.Code, other.Body.String())
	}
	if jobOf(t, other).ID == absorbed.ID {
		t.Fatal("the replacing rebuild must be a new job")
	}
	release()
}

// A FAILING REBUILD MUST ANNOUNCE `error`, not fall silent: the job's error
// is the Index result, published from the machine's one publish point with its
// reason. A rebuild of a branch the repo does not have fails at its first
// step.
func TestRebuild_FailureMarksError(t *testing.T) {
	r, ri := newIndexTestServer(t, "alpha")

	rec, stop := openIndexStream(t, r)
	defer stop()

	resp := postRebuildOn(t, r, "alpha", "no-such-branch")
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST rebuild: got %d, want 201\nbody: %s", resp.Code, resp.Body.String())
	}

	body := rec.waitFor(t, "an error frame", func(b string) bool {
		for _, e := range indexFrames(t, b) {
			if e.Repo == "alpha" && e.State == "error" && e.Reason != "" {
				return true
			}
		}
		return false
	})
	if state := ri.Status().Index.State; state != "error" {
		t.Fatalf("index state = %q, want error (the event must not outrun the state)", state)
	}
	got := states(indexFrames(t, body))
	if len(got) < 2 || got[0] != "indexing" || got[len(got)-1] != "error" {
		t.Fatalf("state sequence = %v, want indexing … error\nbody:\n%s", got, body)
	}
}
