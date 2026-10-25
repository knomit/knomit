package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/events"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// remoteFrames pulls every `event: remote` payload out of an SSE body, in order.
func remoteFrames(t *testing.T, body string) []events.RemoteEvent {
	t.Helper()
	var out []events.RemoteEvent
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l != "event: remote" || i+1 >= len(lines) {
			continue
		}
		data, ok := strings.CutPrefix(lines[i+1], "data: ")
		if !ok {
			continue
		}
		var ev events.RemoteEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("remote frame %q: %v", data, err)
		}
		out = append(out, ev)
	}
	return out
}

// attachAlphaOrigin attaches (or re-attaches) alpha's origin. On the Synchronous
// manager the Sync stage's restart runs exactly one real reconcile round
// before this returns.
func attachAlphaOrigin(t *testing.T, r http.Handler, url, branch string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut, "/repos/alpha/origin",
		strings.NewReader(`{"url":"`+url+`","branch":"`+branch+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT origin (branch %s): %d %s", branch, rec.Code, rec.Body.String())
	}
}

// settleRepoEvents makes "no frame" observable: it starts an index rebuild,
// whose `index` frames are published AFTER everything the test did before, and
// waits for its terminal frame. One subscriber receives in publish order, so
// any `remote` frame published earlier is already in the body it returns.
func settleRepoEvents(t *testing.T, r http.Handler, rec *streamRecorder) string {
	t.Helper()
	before := len(indexFrames(t, rec.body()))
	post := httptest.NewRecorder()
	r.ServeHTTP(post, fromLoopback(httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/index-rebuilds", nil)))
	if post.Code != http.StatusCreated {
		t.Fatalf("start rebuild: %d %s", post.Code, post.Body.String())
	}
	return rec.waitFor(t, "the rebuild's terminal index frame", func(b string) bool {
		evs := indexFrames(t, b)
		for _, e := range evs[min(before, len(evs)):] {
			if e.Repo == "alpha" && (e.State == "ready" || e.State == "error") {
				return true
			}
		}
		return false
	})
}

// The `remote` kind, end to end through real reconcile rounds: the Sync
// stage's own round on each origin attach, against a real file:// origin. It
// is an EDGE: a frame when a persisted status column changes value, silence
// when a round changes nothing. Upstream branch `nope` does not exist on the
// origin, so that round's fetch really fails and persists sync=error.
//
// Sabotage: publish on every round (drop the comparison in
// repos.publishRemoteEdge) → the re-attach round emits a second ok frame →
// red; drop the `remote` case from the handler's switch → no frames → red.
func TestRepoRemoteEvents_EdgesOnly(t *testing.T) {
	originsRoot := t.TempDir()
	url := seedBareKBOn(t, filepath.Join(originsRoot, "alpha.git"), "main")
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: originsRoot},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath: filepath.Join(home, "agent.key"),
		Machine: repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	// A clone, so the agent branch and the origin share history and a healthy
	// round really is healthy. Its own Sync round already ran both halves.
	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "alpha", Mode: "clone",
		Origin: &repos.OriginSpec{URL: url, Branch: "main"}}, nil); err != nil {
		t.Fatalf("clone: %v", err)
	}
	r := (&Server{Manager: m}).NewAPIRouter()

	// The listing carries the row the clone's round wrote.
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if !strings.Contains(listRec.Body.String(), `"remote":{"sync":"ok","push":"ok"}`) {
		t.Fatalf("precondition: GET /repos should list alpha healthy:\n%s", listRec.Body.String())
	}

	rec, stop := openIndexStream(t, r)
	defer stop()

	// THE CONTROL: another healthy round changes no column and says nothing.
	attachAlphaOrigin(t, r, url, "main")
	body := settleRepoEvents(t, r, rec)
	if got := remoteFrames(t, body); len(got) != 0 {
		t.Fatalf("an ok round over an ok row must publish nothing: remote frames = %+v", got)
	}

	// ok → error on the sync half: exactly one frame, the whole row.
	attachAlphaOrigin(t, r, url, "nope")
	body = settleRepoEvents(t, r, rec)
	want := []events.RemoteEvent{{Repo: "alpha", Sync: "error", Push: "ok"}}
	if got := remoteFrames(t, body); !equalRemote(got, want) {
		t.Fatalf("after a failing fetch: remote frames = %+v, want %+v", got, want)
	}

	// error → ok: the recovery edge, one more frame.
	attachAlphaOrigin(t, r, url, "main")
	body = settleRepoEvents(t, r, rec)
	want = append(want, events.RemoteEvent{Repo: "alpha", Sync: "ok", Push: "ok"})
	if got := remoteFrames(t, body); !equalRemote(got, want) {
		t.Fatalf("after recovery: remote frames = %+v, want %+v", got, want)
	}

	// And the listing agrees with the last frame.
	listRec = httptest.NewRecorder()
	r.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if !strings.Contains(listRec.Body.String(), `"remote":{"sync":"ok","push":"ok"}`) {
		t.Fatalf("GET /repos disagrees with the stream:\n%s", listRec.Body.String())
	}
}

func equalRemote(a, b []events.RemoteEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// indexFrames pulls every `event: index` payload out of an SSE body, in order.
func indexFrames(t *testing.T, body string) []events.IndexEvent {
	t.Helper()
	var out []events.IndexEvent
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
		var ev events.IndexEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("index frame %q: %v", data, err)
		}
		out = append(out, ev)
	}
	return out
}

// states reduces a frame list to its state sequence with consecutive
// duplicates collapsed, which is what the UI actually reacts to.
func states(evs []events.IndexEvent) []string {
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
		Cfg:     config.Config{Home: home},
		AgentID: testsigner.FixtureAgentID("machine/test"), AgentBranch: "machine/test",
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

	var mine []events.IndexEvent
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
		Cfg:     config.Config{Home: home},
		AgentID: testsigner.FixtureAgentID("machine/test"), AgentBranch: "machine/test",
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
