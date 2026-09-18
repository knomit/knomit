package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// indexFrames pulls every `event: index` payload out of an SSE body, in order.
func indexFrames(t *testing.T, body string) []repos.IndexEvent {
	t.Helper()
	var out []repos.IndexEvent
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l != "event: index" {
			continue
		}
		if i+1 >= len(lines) {
			continue
		}
		data, ok := strings.CutPrefix(lines[i+1], "data: ")
		if !ok {
			continue
		}
		var ev repos.IndexEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("index frame %q: %v", data, err)
		}
		out = append(out, ev)
	}
	return out
}

// states reduces a frame list to its state sequence with consecutive
// duplicates collapsed, which is what the UI actually reacts to.
func states(evs []repos.IndexEvent) []string {
	var out []string
	for _, e := range evs {
		if len(out) == 0 || out[len(out)-1] != e.State {
			out = append(out, e.State)
		}
	}
	return out
}

// openIndexStream attaches to the server-wide index stream and returns the
// recorder plus a stop func.
func openIndexStream(t *testing.T, r http.Handler) (*streamRecorder, func()) {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index-events", nil).WithContext(reqCtx))
	}()
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })
	return rec, func() { cancel(); <-done }
}

// THE BUG'S OWN PATH: the startup heal in openOne, not the rebuild endpoint.
//
// A server restart opens every repo with markIndexing and heals in the
// background; that flip published nothing, so a UI that snapshotted its repo
// list during the heal showed "indexing" until something else forced a
// refetch. This drives a real repo open — createViaAPI goes through
// Manager.Create → Add → openOne — with the stream attached, and asserts the
// pair of events the UI needs.
//
// Deliberately NOT DisableBackgroundSync: that flag takes the SYNCHRONOUS open
// path (manager.go, the `if b.disableBackgroundSync` branch), which heals inline
// and never calls markIndexing — so a test on that path can only ever observe
// `ready` and would pass against the unpublished bug this fixes.
//
// The `indexing` assertion below is what proves which branch ran, and it was
// verified by mutation rather than assumed: setting DisableBackgroundSync: true
// on this manager makes the sequence `[ready]` and fails the test. If this ever
// starts passing with the flag set, the production path is no longer covered.
func TestRepoIndexEvents_StartupHealEmitsIndexingThenReady(t *testing.T) {
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

	rec, stop := openIndexStream(t, r)
	defer stop()

	createViaAPI(t, r, "alpha")

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

	// And the REST view agrees with what the stream said — the stream must not
	// be able to report a state the endpoint contradicts.
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /repos: %d", listRec.Code)
	}
	if !strings.Contains(listRec.Body.String(), `"index_state":"ready"`) {
		t.Fatalf("repos list disagrees with the stream:\n%s", listRec.Body.String())
	}
}

// THE ERROR TERMINAL. A heal that fails must announce `error`, not fall silent
// — a silent failure leaves the chip on "indexing" forever, which is the stuck
// state this whole mechanism exists to clear, reached by a different route.
//
// Driven through the instance rather than by corrupting a store: inducing a
// real heal failure would test the inducement, and the property under test is
// that the chokepoint publishes on the failure mark.
func TestRepoIndexEvents_FailedHealEmitsError(t *testing.T) {
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
	createViaAPI(t, r, "alpha")

	rec, stop := openIndexStream(t, r)
	defer stop()

	ri := m.Get("alpha")
	if ri == nil {
		t.Fatal("repo alpha missing")
	}
	ri.TestMarkIndexFailed()

	body := rec.waitFor(t, "an error frame", func(b string) bool {
		for _, e := range indexFrames(t, b) {
			if e.Repo == "alpha" && e.State == "error" {
				return true
			}
		}
		return false
	})
	if state, _, _ := ri.IndexStatus(); state != "error" {
		t.Fatalf("IndexStatus = %q, want error (the event must not outrun the state)", state)
	}
	_ = body
}

// THE TERMINAL EVENT MUST SURVIVE THE THROTTLE. Progress is capped at one per
// repo per second; a terminal event inside that window must still arrive,
// because dropping it is not a dropped frame — it is the UI stuck in exactly
// the state this mechanism exists to clear.
//
// Driven through RepoInstance directly rather than a real heal: the point is
// the throttle's behaviour at a boundary a real heal reaches only by timing
// luck.
func TestRepoIndexEvents_TerminalSurvivesTheProgressThrottle(t *testing.T) {
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
	createViaAPI(t, r, "alpha")

	rec, stop := openIndexStream(t, r)
	defer stop()

	ri := m.Get("alpha")
	if ri == nil {
		t.Fatal("repo alpha missing")
	}

	// Production ordering: enter indexing, then progress, then the terminal.
	// It matters — progress does not set the state, so ticking without the
	// entry event would emit frames labelled `ready` and the test would be
	// asserting on a sequence production never produces.
	ri.TestMarkIndexing()
	// A progress tick opens the throttle window, then a second one inside it
	// (which must be suppressed), then the terminal — all well inside a second.
	ri.TestSetIndexProgress(1, 10)
	ri.TestSetIndexProgress(2, 10)
	ri.TestMarkIndexReady()

	body := rec.waitFor(t, "the terminal frame", func(b string) bool {
		for _, e := range indexFrames(t, b) {
			if e.State == "ready" && e.Done == 2 {
				return true
			}
		}
		return false
	})

	// And the suppressed tick really was suppressed: exactly one progress
	// frame, not two. Without this the test would pass against no throttle at
	// all, which is the opposite failure.
	progress := 0
	for _, e := range indexFrames(t, body) {
		if e.State == "indexing" && e.Total == 10 {
			progress++
		}
	}
	if progress != 1 {
		t.Fatalf("got %d progress frames in one throttle window, want 1\nbody:\n%s", progress, body)
	}
}
