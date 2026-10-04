package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo-events", nil).WithContext(reqCtx))
	}()
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })
	return rec, func() { cancel(); <-done }
}

// THE BUG'S OWN PATH: the index job a mount starts, not the rebuild endpoint.
//
// A server restart mounts every repo with its index job in the background;
// that flip used to publish nothing, so a UI that snapshotted its repo list
// during the heal showed "indexing" until something else forced a refetch.
// This drives a real mount — createViaAPI goes through Manager.Create and the
// machine's walk — with the stream attached, and asserts the pair of events
// the UI needs, published from the machine's one publish point.
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

// heldIndexServer is a router over a manager whose repo "alpha" is created
// normally and whose NEXT index job is held at the machine's index-job hook
// until the returned release is called (it is also released at cleanup).
func heldIndexServer(t *testing.T) (http.Handler, *repos.Manager, func()) {
	t.Helper()
	var armed atomic.Bool
	arrived := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	release := make(chan struct{})
	open := func() { releaseOnce.Do(func() { close(release) }) }
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		Machine: repos.Options{Synchronous: true, Hook: func(_ repos.StageID, point string, ctx context.Context) {
			if point != "index-job" || !armed.Load() {
				return
			}
			arriveOnce.Do(func() { close(arrived) })
			select {
			case <-release:
			case <-ctx.Done():
			}
		}},
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { open(); _ = m.Close() })
	s := &Server{Manager: m}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "alpha")

	armed.Store(true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/machine:test/index-rebuilds", nil)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start rebuild: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild never reached the index-job hook")
	}
	return r, m, open
}

// THE ERROR TERMINAL. An index job that ends in error must announce `error`,
// with its reason, not fall silent — a silent failure leaves the chip on
// "indexing" forever. Driven by cancelling a held rebuild through the new
// index:cancel route, so the event, the route's reply and the repo row are
// all checked against one another.
func TestRepoIndexEvents_FailedHealEmitsError(t *testing.T) {
	r, m, _ := heldIndexServer(t)

	rec, stop := openIndexStream(t, r)
	defer stop()

	cancelRec := httptest.NewRecorder()
	r.ServeHTTP(cancelRec, fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/index:cancel", nil)))
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("index:cancel: %d %s", cancelRec.Code, cancelRec.Body.String())
	}
	var row map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &row); err != nil {
		t.Fatalf("index:cancel body: %v", err)
	}
	if row["index_state"] != "error" || row["index_reason"] != "indexing cancelled" {
		t.Fatalf("index:cancel reply = %v, want index_state error, index_reason \"indexing cancelled\"", row)
	}

	rec.waitFor(t, "an error frame with its reason", func(b string) bool {
		for _, e := range indexFrames(t, b) {
			if e.Repo == "alpha" && e.State == "error" && e.Reason == "indexing cancelled" {
				return true
			}
		}
		return false
	})
	if st := m.Get("alpha").Status().Index; st.State != "error" {
		t.Fatalf("Status().Index.State = %q, want error (the event must not outrun the state)", st.State)
	}
}
