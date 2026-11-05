package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/events"
)

// The job routes read the repo's TaskHub and nothing else. These fixtures run
// REAL hub tasks: a rebuild-op task that finished (rebuild-1) and one held
// running (rebuild-2) until the test releases it. A hub keeps an op's running
// task and its last terminal one, so these are all it can hold of the op: two
// states and ids that differ, so a route answering the wrong job or an
// invented one fails.

// jobEnvelope is the job body these tests decode: the wire shape api/jobs
// writes, declared here because api/jobs exports nothing but Mount.
type jobEnvelope struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// decodeStrict decodes b into v and fails on a field v does not declare, so a
// field added to the job wire shape is a red test, not a silent drop.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type jobsFixture struct {
	r       http.Handler
	hub     *events.TaskHub
	release func()               // the held task ends done
	fail    func(message string) // the held task ends in error
	done    string               // the finished rebuild task
	running string               // the held rebuild task
}

func newJobsFixture(t *testing.T) jobsFixture {
	t.Helper()
	hub, pub := events.NewTaskHub(context.Background())
	t.Cleanup(pub.Shutdown)
	m := newTestManagerWithRepos(t, "empty") // a repo whose hub is nil
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", AgentBranch: "agent/test", Hub: hub,
	}))
	m.Set("beta", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "beta", AgentBranch: "agent/test", Hub: testHub(),
	}))

	f := jobsFixture{r: (&Server{Manager: m}).NewAPIRouter(), hub: hub}
	f.done = runToEnd(t, hub, "rebuild", events.TaskEvent{Status: "done", Message: "rebuild complete"})

	gate := make(chan struct{})
	failWith := make(chan string, 1)
	started := make(chan struct{})
	id, err := hub.Start("rebuild", func(ctx context.Context, emit func(events.TaskEvent)) {
		emit(events.TaskEvent{Status: "running", Phase: "start", Message: "rebuild starting"})
		close(started)
		select {
		case <-gate:
			emit(events.TaskEvent{Status: "done", Message: "rebuild complete"})
		case msg := <-failWith:
			emit(events.TaskEvent{Status: "error", Message: msg})
		case <-ctx.Done():
			emit(events.TaskEvent{Status: "error", Message: ctx.Err().Error()})
		}
	})
	if err != nil {
		t.Fatalf("start held rebuild: %v", err)
	}
	<-started
	f.running = id
	var once bool
	f.release = func() {
		if !once {
			once = true
			close(gate)
		}
	}
	f.fail = func(msg string) { failWith <- msg }
	t.Cleanup(f.release)
	if f.done == f.running {
		t.Fatalf("fixture ids collide: %s %s", f.done, f.running)
	}
	return f
}

// runToEnd runs one hub task of op that emits final at once, and waits until
// the hub records it as op's last terminal task.
func runToEnd(t *testing.T, hub *events.TaskHub, op string, final events.TaskEvent) string {
	t.Helper()
	id, err := hub.Start(op, func(_ context.Context, emit func(events.TaskEvent)) { emit(final) })
	if err != nil {
		t.Fatalf("start %s: %v", op, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if ev, ok := hub.Task(id); ok && ev.Status == final.Status {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never reached %s", id, final.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f jobsFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(method, path, nil)))
	return rec
}

func requireContentType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != want {
		t.Fatalf("content-type = %q, want %q (body %s)", got, want, rec.Body.String())
	}
}

func requireJobProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, title, detail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	requireContentType(t, rec, "application/problem+json")
	gotTitle, gotDetail := decodeProblem(t, rec)
	if gotTitle != title || gotDetail != detail {
		t.Fatalf("problem = %q / %q, want %q / %q", gotTitle, gotDetail, title, detail)
	}
}

type jobsListBody struct {
	Count int `json:"count"`
	Links map[string]struct {
		Href string `json:"href"`
	} `json:"_links"`
	Embedded struct {
		Jobs []jobEnvelope `json:"jobs"`
	} `json:"_embedded"`
}

func listJobs(t *testing.T, f jobsFixture, path string) jobsListBody {
	t.Helper()
	rec := f.do(t, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, "application/hal+json")
	if !strings.Contains(rec.Body.String(), `"jobs":[`) {
		t.Fatalf("jobs must be an array, never null or absent: %s", rec.Body.String())
	}
	var body jobsListBody
	if err := decodeStrict(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body.String())
	}
	if body.Links["self"].Href != path {
		t.Fatalf("self link = %q, want %q", body.Links["self"].Href, path)
	}
	if body.Count != len(body.Embedded.Jobs) {
		t.Fatalf("count %d disagrees with %d jobs", body.Count, len(body.Embedded.Jobs))
	}
	return body
}

const alphaRebuild = "/repos/alpha/branches/agent:test/index-rebuilds"

// LIST is what the hub holds of the rebuild op: the running job first, then
// the last finished one.
func TestJobs_ListIsTheHubsRunningAndLastFinishedJob(t *testing.T) {
	f := newJobsFixture(t)

	got := listJobs(t, f, alphaRebuild)
	want := []jobEnvelope{
		{ID: f.running, Kind: "index-rebuild", State: "running"},
		{ID: f.done, Kind: "index-rebuild", State: "done"},
	}
	if len(got.Embedded.Jobs) != len(want) {
		t.Fatalf("index-rebuilds = %+v, want %+v", got.Embedded.Jobs, want)
	}
	for i := range want {
		if got.Embedded.Jobs[i] != want[i] {
			t.Fatalf("index-rebuilds[%d] = %+v, want %+v", i, got.Embedded.Jobs[i], want[i])
		}
	}

	// A hub that ran nothing, and a repo with no hub at all, list nothing.
	for _, path := range []string{
		"/repos/beta/branches/agent:test/index-rebuilds",
		"/repos/empty/branches/agent:test/index-rebuilds",
	} {
		if got := listJobs(t, f, path); got.Count != 0 {
			t.Fatalf("%s = %+v, want an empty list", path, got)
		}
	}
}

// GET answers the hub's job by id: a known id with its own state, an id the
// hub never held is 404.
func TestJobs_GetReadsTheHubByID(t *testing.T) {
	f := newJobsFixture(t)

	for _, c := range []struct {
		path string
		want jobEnvelope
	}{
		{alphaRebuild + "/" + f.running, jobEnvelope{ID: f.running, Kind: "index-rebuild", State: "running"}},
		{alphaRebuild + "/" + f.done, jobEnvelope{ID: f.done, Kind: "index-rebuild", State: "done"}},
	} {
		rec := f.do(t, http.MethodGet, c.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", c.path, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, "application/hal+json")
		if got := jobOf(t, rec); got != c.want {
			t.Fatalf("GET %s = %+v, want %+v", c.path, got, c.want)
		}
	}

	requireJobProblem(t, f.do(t, http.MethodGet, alphaRebuild+"/rebuild-99"),
		http.StatusNotFound, "Job not found", "job rebuild-99 not found")
	requireJobProblem(t, f.do(t, http.MethodGet, "/repos/empty/branches/agent:test/index-rebuilds/"+f.done),
		http.StatusNotFound, "Job not found", "job "+f.done+" not found")
}

// waitJob polls GET path until the job's state is want, and returns it.
func waitJob(t *testing.T, f jobsFixture, path, want string) jobEnvelope {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := f.do(t, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
		}
		got := jobOf(t, rec)
		if got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s = %+v, never reached %s", path, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// DELETE refuses what it cannot cancel and touches nothing: a finished job is
// 409, an id the hub does not hold 404. The cancel itself goes through the
// lifecycle machine and is TestJobs_DeleteCancelsALifecycleRebuild's.
func TestJobs_DeleteRefusesAFinishedOrUnknownJob(t *testing.T) {
	f := newJobsFixture(t)

	requireJobProblem(t, f.do(t, http.MethodDelete, alphaRebuild+"/rebuild-99"),
		http.StatusNotFound, "Job not found", "job rebuild-99 not found")
	requireJobProblem(t, f.do(t, http.MethodDelete, alphaRebuild+"/"+f.done),
		http.StatusConflict, "Job already finished", "job "+f.done+" has already finished; there is nothing to cancel")
	if got := waitJob(t, f, alphaRebuild+"/"+f.running, "running"); got.ID != f.running {
		t.Fatalf("refused DELETEs must not touch the running job: %+v", got)
	}
}

// serveWithin runs one request on a stream recorder and fails the test if the
// handler has not returned within limit: a stream that never ends is a hang,
// and this is where it shows, fast.
func serveWithin(t *testing.T, r http.Handler, path string, limit time.Duration) *streamRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	select {
	case <-done:
		return rec
	case <-time.After(limit):
		cancel()
		<-done
		t.Fatalf("GET %s did not end within %v; body so far:\n%s", path, limit, rec.body())
		return nil
	}
}

func taskFrames(t *testing.T, body string) []events.TaskEvent {
	t.Helper()
	var out []events.TaskEvent
	for _, frame := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(frame, "event: task\ndata: ") {
			continue
		}
		var ev events.TaskEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: task\ndata: ")), &ev); err != nil {
			t.Fatalf("task frame %q: %v", frame, err)
		}
		out = append(out, ev)
	}
	return out
}

// EVENTS for an id the hub does not hold is the documented 404 before any
// stream opens — not a stream that never ends.
func TestJobs_EventsForAnUnknownJobIs404NotAHang(t *testing.T) {
	f := newJobsFixture(t)
	for _, c := range []struct{ path, id string }{
		{alphaRebuild + "/rebuild-99/events", "rebuild-99"},
	} {
		rec := serveWithin(t, f.r, c.path, 2*time.Second)
		requireJobProblem(t, rec.ResponseRecorder, http.StatusNotFound, "Job not found", "job "+c.id+" not found")
	}
}

// EVENTS for a finished job is its terminal frame, and the stream ends.
func TestJobs_EventsForAFinishedJobEndsAtOnce(t *testing.T) {
	f := newJobsFixture(t)
	rec := serveWithin(t, f.r, alphaRebuild+"/"+f.done+"/events", 2*time.Second)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.body())
	}
	requireContentType(t, rec.ResponseRecorder, "text/event-stream")
	frames := taskFrames(t, rec.body())
	if len(frames) != 1 || frames[0].ID != f.done || frames[0].Status != "done" {
		t.Fatalf("frames = %+v, want the one done frame of %s", frames, f.done)
	}
}

// EVENTS for a running job follows it and ends with its terminal frame.
func TestJobs_EventsForARunningJobEndsWithItsTerminal(t *testing.T) {
	f := newJobsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, alphaRebuild+"/"+f.running+"/events", nil).WithContext(ctx))
	}()
	rec.waitFor(t, "the running frame", func(b string) bool { return strings.Contains(b, `"status":"running"`) })
	f.release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream did not end after the job finished; body:\n%s", rec.body())
	}
	frames := taskFrames(t, rec.body())
	if len(frames) < 2 || frames[0].Status != "running" || frames[len(frames)-1].Status != "done" {
		t.Fatalf("frames = %+v, want running … done", frames)
	}
	for _, ev := range frames {
		if ev.ID != f.running {
			t.Fatalf("frame of another job on %s's stream: %+v", f.running, ev)
		}
	}
}

// openJobStream opens path's events stream and waits for the running frame.
func openJobStream(t *testing.T, r http.Handler, path string) (*streamRecorder, <-chan struct{}, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	rec.waitFor(t, "the running frame", func(b string) bool { return strings.Contains(b, `"status":"running"`) })
	return rec, done, func() { cancel(); <-done }
}

func requireStreamEnds(t *testing.T, rec *streamRecorder, done <-chan struct{}, id, status, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream did not end after the job's %s; body:\n%s", status, rec.body())
	}
	frames := taskFrames(t, rec.body())
	last := frames[len(frames)-1]
	if frames[0].Status != "running" || last.ID != id || last.Status != status || last.Message != message {
		t.Fatalf("frames = %+v, want running … %s %q", frames, status, message)
	}
}

// The idle-stream keepalive test lives with the keepalive interval, in
// api/jobs (its var is private to that package).

// A RUNNING job that ends in ERROR ends its stream too, not only done.
func TestJobs_EventsForARunningJobEndsOnAnErrorTerminal(t *testing.T) {
	f := newJobsFixture(t)
	rec, done, stop := openJobStream(t, f.r, alphaRebuild+"/"+f.running+"/events")
	defer stop()
	f.fail("rebuild failed: disk full")
	requireStreamEnds(t, rec, done, f.running, "error", "rebuild failed: disk full")
}

// A finished job whose op has RUN AGAIN is still the hub's last terminal of
// the op, though the Subscribe snapshot no longer shows it (it holds the
// running one): its stream is its terminal frame and ends.
func TestJobs_EventsForAFinishedJobWhileItsOpRunsAgain(t *testing.T) {
	f := newJobsFixture(t) // rebuild-1 done, rebuild-2 running
	rec := serveWithin(t, f.r, alphaRebuild+"/"+f.done+"/events", 2*time.Second)
	frames := taskFrames(t, rec.body())
	if rec.Code != http.StatusOK || len(frames) != 1 || frames[0].ID != f.done || frames[0].Status != "done" {
		t.Fatalf("status %d frames %+v, want the one done frame of %s", rec.Code, frames, f.done)
	}
}

// THE PRODUCTION PATH: a rebuild started through POST is recorded by the
// lifecycle machine in the hub, and the list, get and delete routes see it
// while it runs — no registry is involved.
func TestJobs_ALifecycleRebuildIsListedAndReadWhileItRuns(t *testing.T) {
	r, _, release := heldIndexServer(t)
	f := jobsFixture{r: r}
	const base = "/repos/alpha/branches/machine:test/index-rebuilds"

	list := listJobs(t, f, base)
	if len(list.Embedded.Jobs) == 0 || list.Embedded.Jobs[0].State != "running" || list.Embedded.Jobs[0].Kind != "index-rebuild" {
		t.Fatalf("index-rebuilds while one runs = %+v, want the running rebuild first", list.Embedded.Jobs)
	}
	id := list.Embedded.Jobs[0].ID

	rec := f.do(t, http.MethodGet, base+"/"+id)
	if got := jobOf(t, rec); rec.Code != http.StatusOK || got.ID != id || got.State != "running" {
		t.Fatalf("GET %s = %d %+v", id, rec.Code, got)
	}

	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := jobOf(t, f.do(t, http.MethodGet, base+"/"+id))
		if got.State == "done" {
			break
		}
		if got.State != "running" || time.Now().After(deadline) {
			t.Fatalf("rebuild %s after release = %+v, want done", id, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// DELETE OF A RUNNING REBUILD is CancelIndex, the lifecycle machine's own
// cancel (the one POST /repos/{repo}/index:cancel sends): 202 once it has
// drained, the job then error "indexing cancelled", its stream one terminal
// frame, the list showing it finished, a second DELETE 409.
func TestJobs_DeleteCancelsALifecycleRebuild(t *testing.T) {
	r, _, _ := heldIndexServer(t) // the rebuild stays held: only the cancel can end it
	f := jobsFixture{r: r}
	const base = "/repos/alpha/branches/machine:test/index-rebuilds"
	list := listJobs(t, f, base)
	if len(list.Embedded.Jobs) == 0 || list.Embedded.Jobs[0].State != "running" {
		t.Fatalf("index-rebuilds = %+v, want the held rebuild running", list.Embedded.Jobs)
	}
	id := list.Embedded.Jobs[0].ID

	rec := f.do(t, http.MethodDelete, base+"/"+id)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE running rebuild = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	want := jobEnvelope{ID: id, Kind: "index-rebuild", State: "error", Error: repos.ErrIndexCancelled.Error()}
	if got := jobOf(t, rec); got != want {
		t.Fatalf("202 body = %+v, want %+v", got, want)
	}
	if got := waitJob(t, f, base+"/"+id, "error"); got != want {
		t.Fatalf("GET after cancel = %+v, want %+v", got, want)
	}
	if got := listJobs(t, f, base); len(got.Embedded.Jobs) != 1 || got.Embedded.Jobs[0] != want {
		t.Fatalf("list after cancel = %+v, want [%+v]", got.Embedded.Jobs, want)
	}
	ev := serveWithin(t, r, base+"/"+id+"/events", 2*time.Second)
	if frames := taskFrames(t, ev.body()); len(frames) != 1 || frames[0].Status != "error" {
		t.Fatalf("events after cancel = %+v, want one error frame", frames)
	}
	requireJobProblem(t, f.do(t, http.MethodDelete, base+"/"+id),
		http.StatusConflict, "Job already finished", "job "+id+" has already finished; there is nothing to cancel")
}

// testHub is a task hub whose publisher the test does not need.
func testHub() *events.TaskHub {
	h, _ := events.NewTaskHub(context.Background())
	return h
}
